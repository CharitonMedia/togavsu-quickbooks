// Package fakecore is a stand-in for the Togavsu core API, built from the operations the
// module uses (spec section 7 and CORE-01, CORE-02, CORE-03, CORE-06 in CORE-CHANGES.md).
// It keeps synthetic money records in memory, honors matter walls for the books reader and
// for viewers, signs and delivers events to the module, and keeps one id row per core
// record in the test database's core schema so the module's foreign keys hold. It is test
// scaffolding: the real core replaces it in the module-core lane.
package fakecore

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	quickbooks "github.com/CharitonMedia/togavsu-quickbooks"
	"github.com/CharitonMedia/togavsu-quickbooks/internal/wire"
)

// User is a firm user.
type User struct {
	ID      string
	Name    string
	Roles   []string
	EmailOn bool
}

// MatterRec is a matter with its wall.
type MatterRec struct {
	quickbooks.BooksMatter
	Walled  bool
	Members map[string]bool
}

// Notification is one notification the module sent, as delivered.
type Notification struct {
	Kind, Title, Link string
	Roles             []string
	Recipients        []string
	Emailed           []string
}

// Event is one delivered or dropped event.
type Event struct {
	Payload   wire.EventPayload
	Delivered bool
	Status    int
}

// Core is the fake core.
type Core struct {
	mu sync.Mutex

	DB            *sql.DB
	Token         string
	WebhookSecret string
	Now           func() time.Time
	// Module receives events; set it once the module server exists.
	Module http.Handler
	// Muted drops events (to prove the nightly reconcile finds them).
	Muted bool

	Profile  quickbooks.Profile
	Granted  []string
	Settings map[string]string
	Secrets  map[string]string

	users         map[string]*User
	contacts      map[string]*quickbooks.BooksContact
	matters       map[string]*MatterRec
	invoices      map[string]*quickbooks.Invoice
	payments      map[string]*quickbooks.Payment
	trustAccounts map[string]*quickbooks.TrustAccount
	entries       map[string]*quickbooks.TrustEntry
	expenses      map[string]*quickbooks.Expense
	order         []string // record ids in creation order
	tokens        map[string]ctxToken

	Notifications []Notification
	Events        []Event
	// Requests lists "METHOD path ctx|service" for every API call.
	Requests []string
	// SecretWrites lists the secret names written.
	SecretWrites []string

	srv *httptest.Server
}

type ctxToken struct {
	user, matter string
}

// DefaultSettings are the manifest defaults the core renders on the settings form.
func DefaultSettings() map[string]string {
	return map[string]string{
		"qbo_client_id": "", "qbo_environment": "sandbox", "sync_start_date": "",
		"account_operating_bank": "", "account_trust_bank": "", "account_trust_liability": "Client Trust Liability",
		"account_earned_from_trust": "Legal Fee Income", "payment_deposit": "undeposited_funds", "expense_push": "invoice_lines_only",
		"account_client_costs": "Client Costs Advanced", "account_expense_paid_from": "",
		"item_legal_services": "Legal Services", "item_flat_fee": "Flat Fee", "item_reimbursed_costs": "Reimbursed Costs",
		"item_adjustment": "Invoice Adjustment", "item_interest": "Late Interest", "item_write_off": "Write-Off",
		"reconcile_hour_utc": "8", "retry_schedule": "1m,5m,30m,2h,6h", "alert_interval_minutes": "15",
	}
}

// New starts a fake core on 127.0.0.1. db is an admin connection to the test database.
func New(db *sql.DB, now func() time.Time) *Core {
	c := &Core{DB: db, Now: now, Token: "mod_" + randHex(tokenBytes), WebhookSecret: "whsec_" + randHex(tokenBytes),
		Profile:  quickbooks.Profile{FirmName: "Sample Law Co", TimeZone: "America/Chicago", Currency: "USD"},
		Settings: DefaultSettings(), Secrets: map[string]string{},
		users: map[string]*User{}, contacts: map[string]*quickbooks.BooksContact{}, matters: map[string]*MatterRec{},
		invoices: map[string]*quickbooks.Invoice{}, payments: map[string]*quickbooks.Payment{},
		trustAccounts: map[string]*quickbooks.TrustAccount{}, entries: map[string]*quickbooks.TrustEntry{},
		expenses: map[string]*quickbooks.Expense{}, tokens: map[string]ctxToken{}}
	c.srv = httptest.NewServer(http.HandlerFunc(c.serve))
	return c
}

// URL is the core's base address.
func (c *Core) URL() string { return c.srv.URL }

// Close stops the server.
func (c *Core) Close() { c.srv.Close() }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newID is a UUIDv7-shaped id.
func (c *Core) newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	ms := uint64(c.Now().UnixMilli())
	binary.BigEndian.PutUint16(b[:uuidMillisHigh], uint16(ms>>32))
	binary.BigEndian.PutUint32(b[uuidMillisHigh:uuidMillisEnd], uint32(ms))
	b[uuidVersionByte] = (b[uuidVersionByte] & 0x0f) | 0x70
	b[uuidVariantByte] = (b[uuidVariantByte] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	var out strings.Builder
	from := zero
	for _, at := range uuidDashes {
		out.WriteString(s[from:at])
		out.WriteByte('-')
		from = at
	}
	out.WriteString(s[from:])
	return out.String()
}

func (c *Core) stub(table, id string) {
	if _, err := c.DB.Exec(`INSERT INTO core.`+table+` (id) VALUES ($1)`, id); err != nil {
		panic("fakecore: " + err.Error())
	}
}

// ---- setup of synthetic records ----

// AddUser adds a firm user with email notifications on.
func (c *Core) AddUser(name string, roles ...string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.newID()
	c.users[id] = &User{ID: id, Name: name, Roles: roles, EmailOn: true}
	c.stub("users", id)
	return id
}

// SetEmail turns a user's email preference for sync_problem on or off.
func (c *Core) SetEmail(userID string, on bool) {
	c.mu.Lock()
	c.users[userID].EmailOn = on
	c.mu.Unlock()
}

// AddContact adds a contact.
func (c *Core) AddContact(b quickbooks.BooksContact) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	b.ID = c.newID()
	if b.Kind == "" {
		b.Kind = "person"
	}
	c.contacts[b.ID] = &b
	c.stub("contacts", b.ID)
	return b.ID
}

// UpdateContact changes a contact and emits contact.updated.
func (c *Core) UpdateContact(id string, edit func(*quickbooks.BooksContact)) {
	c.mu.Lock()
	edit(c.contacts[id])
	c.mu.Unlock()
	c.emit("contact.updated", "contact", id, "")
}

// AddMatter adds an open matter for a client.
func (c *Core) AddMatter(number, name, clientID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.newID()
	c.matters[id] = &MatterRec{BooksMatter: quickbooks.BooksMatter{ID: id, Number: number, Name: name, Status: "open", ClientContactID: clientID},
		Members: map[string]bool{}}
	c.stub("matters", id)
	return id
}

// Wall walls a matter to its members; Unwall lifts it.
func (c *Core) Wall(matterID string, members ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.matters[matterID]
	m.Walled, m.Members = true, map[string]bool{}
	for _, u := range members {
		m.Members[u] = true
	}
}

// Unwall lifts a wall.
func (c *Core) Unwall(matterID string) {
	c.mu.Lock()
	c.matters[matterID].Walled = false
	c.mu.Unlock()
}

// AddTrustAccount adds a trust bank account.
func (c *Core) AddTrustAccount(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.newID()
	c.trustAccounts[id] = &quickbooks.TrustAccount{ID: id, Name: name, Bank: "Sample Bank", LastFour: "0042", Active: true}
	c.stub("trust_accounts", id)
	return id
}

// ContextToken issues a context token for a user, on a matter or none.
func (c *Core) ContextToken(userID, matterID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := "ctx_" + randHex(tokenBytes)
	c.tokens[t] = ctxToken{user: userID, matter: matterID}
	return t
}

// ---- money operations (as the core's own screens and services would make them) ----

// InvoiceInput is a new invoice.
type InvoiceInput struct {
	MatterID        string
	Number          string
	IssuedOn        string
	Lines           []quickbooks.InvoiceLine
	AdjustmentCents int64
	AdjustmentNote  string
	InterestCents   int64
	// TotalCents overrides the computed total (to test totals_mismatch).
	TotalCents *int64
}

// CreateInvoice adds a draft invoice and emits invoice.created.
func (c *Core) CreateInvoice(in InvoiceInput) string {
	c.mu.Lock()
	id := c.newID()
	inv := &quickbooks.Invoice{ID: id, MatterID: in.MatterID, Number: in.Number, IssuedOn: in.IssuedOn, Status: "draft",
		Lines: in.Lines, AdjustmentCents: in.AdjustmentCents, AdjustmentNote: in.AdjustmentNote, InterestCents: in.InterestCents}
	for _, l := range in.Lines {
		inv.TotalCents += l.AmountCents
	}
	inv.TotalCents += in.AdjustmentCents + in.InterestCents
	if in.TotalCents != nil {
		inv.TotalCents = *in.TotalCents
	}
	inv.BalanceDueCents = inv.TotalCents
	c.invoices[id] = inv
	c.order = append(c.order, id)
	c.stub("invoices", id)
	c.mu.Unlock()
	c.emit("invoice.created", "invoice", id, in.MatterID)
	return id
}

// SendInvoice marks an invoice sent and emits invoice.sent.
func (c *Core) SendInvoice(id string) {
	c.mu.Lock()
	c.invoices[id].Status = "sent"
	m := c.invoices[id].MatterID
	c.mu.Unlock()
	c.emit("invoice.sent", "invoice", id, m)
}

// EditInvoice changes an invoice and emits invoice.updated.
func (c *Core) EditInvoice(id string, edit func(*quickbooks.Invoice)) {
	c.mu.Lock()
	inv := c.invoices[id]
	edit(inv)
	c.recalc(inv)
	m := inv.MatterID
	c.mu.Unlock()
	c.emit("invoice.updated", "invoice", id, m)
}

// WriteOff sets an invoice's write-off.
func (c *Core) WriteOff(id string, cents int64, reason string) {
	c.EditInvoice(id, func(inv *quickbooks.Invoice) { inv.WriteOffCents, inv.WriteOffReason = cents, reason })
}

// VoidInvoice voids an invoice and emits invoice.voided.
func (c *Core) VoidInvoice(id string) {
	c.mu.Lock()
	c.invoices[id].Status = "void"
	m := c.invoices[id].MatterID
	c.mu.Unlock()
	c.emit("invoice.voided", "invoice", id, m)
}

// recalc sets an invoice's paid and balance from its completed payments.
func (c *Core) recalc(inv *quickbooks.Invoice) {
	var paid int64
	for _, p := range c.payments {
		if p.InvoiceID != nil && *p.InvoiceID == inv.ID && p.Status == "completed" {
			paid += p.AmountCents
		}
	}
	inv.AmountPaidCents = paid
	inv.BalanceDueCents = inv.TotalCents - paid - inv.WriteOffCents
	if inv.BalanceDueCents <= zero64 && inv.Status != "void" && inv.Status != "draft" {
		inv.Status = "paid"
	} else if inv.Status == "paid" {
		inv.Status = "sent"
	}
}

// SetImportedPaid sets an invoice's amount paid with no payment records (an import).
func (c *Core) SetImportedPaid(id string, cents int64) {
	c.mu.Lock()
	inv := c.invoices[id]
	inv.AmountPaidCents = cents
	inv.BalanceDueCents = inv.TotalCents - cents - inv.WriteOffCents
	m := inv.MatterID
	c.mu.Unlock()
	c.emit("invoice.updated", "invoice", id, m)
}

func (c *Core) clientOf(matterID string) string { return c.matters[matterID].ClientContactID }

// PaymentInput is a new payment.
type PaymentInput struct {
	MatterID, InvoiceID, Method, Reference string
	AmountCents                            int64
	Destination                            string // operating (default) or trust
	TrustAccountID                         string
	Pending                                bool
}

// RecordPayment adds a payment. An operating payment completes at once and updates its
// invoice; a trust payment also posts its ledger deposit.
func (c *Core) RecordPayment(in PaymentInput) string {
	c.mu.Lock()
	id := c.newID()
	p := &quickbooks.Payment{ID: id, CreatedAt: c.Now().UTC(), ContactID: c.clientOf(in.MatterID), MatterID: in.MatterID,
		AmountCents: in.AmountCents, Destination: in.Destination, Method: in.Method, ReferenceNumber: in.Reference, Status: "completed"}
	if p.Destination == "" {
		p.Destination = "operating"
	}
	if p.Method == "" {
		p.Method = "check"
	}
	if in.Pending {
		p.Status = "pending"
	}
	if in.InvoiceID != "" {
		inv := in.InvoiceID
		p.InvoiceID = &inv
	}
	var entry string
	if p.Destination == "trust" {
		acct := in.TrustAccountID
		p.TrustAccountID = &acct
		entry = c.newEntry(in.MatterID, acct, "deposit", in.AmountCents, "Client payment into trust", nil)
		p.TrustEntryID = &entry
	}
	c.payments[id] = p
	c.order = append(c.order, id)
	c.stub("payments", id)
	if p.InvoiceID != nil {
		c.recalc(c.invoices[*p.InvoiceID])
	}
	c.mu.Unlock()
	c.emit("payment.created", "payment", id, in.MatterID)
	if p.Status == "completed" {
		c.emit("payment.completed", "payment", id, in.MatterID)
	}
	if entry != "" {
		c.emit("trust.entry_posted", "trust_entry", entry, in.MatterID)
	}
	if p.InvoiceID != nil {
		c.emit("invoice.updated", "invoice", *p.InvoiceID, in.MatterID)
	}
	return id
}

// ReversePayment reverses a payment.
func (c *Core) ReversePayment(id string) {
	c.mu.Lock()
	p := c.payments[id]
	p.Status = "reversed"
	if p.InvoiceID != nil {
		c.recalc(c.invoices[*p.InvoiceID])
	}
	m := p.MatterID
	c.mu.Unlock()
	c.emit("payment.reversed", "payment", id, m)
}

func (c *Core) newEntry(matterID, acct, kind string, cents int64, desc string, reverses *string) string {
	id := c.newID()
	c.entries[id] = &quickbooks.TrustEntry{ID: id, CreatedAt: c.Now().UTC(), TrustAccountID: acct, ClientID: c.clientOf(matterID),
		MatterID: matterID, EntryType: kind, AmountCents: cents, Description: desc, ReversesEntryID: reverses, Method: "check"}
	c.order = append(c.order, id)
	c.stub("trust_ledger", id)
	return id
}

// PostTrust posts a ledger entry (deposit, withdrawal, refund, adjustment) with no
// payment. Adjustments carry their sign.
func (c *Core) PostTrust(matterID, acct, kind string, cents int64, desc string) string {
	c.mu.Lock()
	id := c.newEntry(matterID, acct, kind, cents, desc, nil)
	c.mu.Unlock()
	c.emit("trust.entry_posted", "trust_entry", id, matterID)
	return id
}

// Reverse posts the reversal of an entry.
func (c *Core) Reverse(entryID string) string {
	c.mu.Lock()
	o := c.entries[entryID]
	orig := o.ID
	id := c.newEntry(o.MatterID, o.TrustAccountID, "reversal", -o.Signed(), "Reversal of "+o.Description, &orig)
	m := o.MatterID
	c.mu.Unlock()
	c.emit("trust.entry_posted", "trust_entry", id, m)
	return id
}

// ApplyTrust pays an invoice from trust: one transfer to operating and one operating
// payment linked to it, written together.
func (c *Core) ApplyTrust(invoiceID, acct string, cents int64) (entryID, paymentID string) {
	c.mu.Lock()
	inv := c.invoices[invoiceID]
	m := inv.MatterID
	entryID = c.newEntry(m, acct, "transfer_to_operating", cents, "Applied to invoice "+inv.Number, nil)
	paymentID = c.newID()
	e, iv := entryID, invoiceID
	c.payments[paymentID] = &quickbooks.Payment{ID: paymentID, CreatedAt: c.Now().UTC(), ContactID: c.clientOf(m), MatterID: m,
		AmountCents: cents, Destination: "operating", Method: "internal_transfer", Status: "completed", InvoiceID: &iv, TrustEntryID: &e}
	c.order = append(c.order, paymentID)
	c.stub("payments", paymentID)
	c.recalc(inv)
	c.mu.Unlock()
	c.emit("trust.entry_posted", "trust_entry", entryID, m)
	c.emit("payment.completed", "payment", paymentID, m)
	c.emit("invoice.updated", "invoice", invoiceID, m)
	return entryID, paymentID
}

// AddExpense adds an expense and emits expense.created.
func (c *Core) AddExpense(matterID, date, desc string, cents int64, billable bool) string {
	c.mu.Lock()
	id := c.newID()
	c.expenses[id] = &quickbooks.Expense{ID: id, MatterID: matterID, IncurredOn: date, Description: desc, AmountCents: cents, Billable: billable}
	c.order = append(c.order, id)
	c.stub("expenses", id)
	c.mu.Unlock()
	c.emit("expense.created", "expense", id, matterID)
	return id
}

// Invoice returns a copy of a core invoice.
func (c *Core) Invoice(id string) quickbooks.Invoice {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.invoices[id]
}

// Payment returns a copy of a core payment.
func (c *Core) Payment(id string) quickbooks.Payment {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.payments[id]
}

// HardDeleteContact deletes a contact row; a module foreign key refuses it, and the error
// names the module whose schema holds the constraint.
func (c *Core) HardDeleteContact(id string) error {
	_, err := c.DB.Exec(`DELETE FROM core.contacts WHERE id = $1`, id)
	if err == nil {
		c.mu.Lock()
		delete(c.contacts, id)
		c.mu.Unlock()
		return nil
	}
	var schema string
	if e := c.DB.QueryRow(`SELECT n.nspname FROM pg_constraint k JOIN pg_namespace n ON n.oid = k.connamespace
WHERE k.contype = 'f' AND k.confrelid = 'core.contacts'::regclass AND n.nspname LIKE 'mod\_%'
ORDER BY n.nspname LIMIT 1`).Scan(&schema); e == nil {
		return fmt.Errorf("contact is still referenced by module %s: %w", strings.TrimPrefix(schema, "mod_"), err)
	}
	return err
}

// ---- events ----

func (c *Core) walled(matterID string) bool {
	m := c.matters[matterID]
	return m != nil && m.Walled
}

// emit signs an event and delivers it to the module, unless the record sits on a walled
// matter (the module's subscription runs as the books reader) or events are muted.
func (c *Core) emit(action, recordType, recordID, matterID string) {
	c.mu.Lock()
	ev := wire.EventPayload{ID: "evt_" + randHex(eventIDBytes), Action: action, RecordType: recordType, RecordID: recordID,
		MatterID: matterID, OccurredAt: c.Now().UTC().Format(time.RFC3339Nano), Module: quickbooks.ID}
	drop := c.Muted || c.Module == nil || (matterID != "" && c.walled(matterID))
	if action == "contact.updated" && c.contactHidden(recordID) {
		drop = true
	}
	c.mu.Unlock()
	if drop {
		c.mu.Lock()
		c.Events = append(c.Events, Event{Payload: ev})
		c.mu.Unlock()
		return
	}
	c.Deliver(ev)
}

// Deliver signs and posts one event to the module, as the core's dispatcher does.
func (c *Core) Deliver(ev wire.EventPayload) int {
	body, _ := json.Marshal(ev)
	req := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(body))
	req.Header.Set(wire.SignatureHeader, wire.SignWebhook(c.WebhookSecret, c.Now(), body))
	rec := httptest.NewRecorder()
	c.Module.ServeHTTP(rec, req)
	c.mu.Lock()
	c.Events = append(c.Events, Event{Payload: ev, Delivered: true, Status: rec.Code})
	c.mu.Unlock()
	return rec.Code
}

// Delivered lists the delivered events.
func (c *Core) Delivered() []wire.EventPayload {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []wire.EventPayload
	for _, e := range c.Events {
		if e.Delivered {
			out = append(out, e.Payload)
		}
	}
	return out
}

// ---- the API ----

func (c *Core) fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}

func send(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// principal is who a call runs as: a viewer (context token) or the books reader.
type principal struct {
	viewer *User
	matter string
}

func (p principal) books() bool { return p.viewer == nil }

func (c *Core) contactHidden(id string) bool {
	any, open := false, false
	for _, m := range c.matters {
		if m.ClientContactID == id {
			any = true
			if !m.Walled {
				open = true
			}
		}
	}
	return any && !open
}

// sees reports whether p may see a record on matterID.
func (c *Core) sees(p principal, matterID string) bool {
	m := c.matters[matterID]
	if m == nil {
		return false
	}
	if !m.Walled {
		return true
	}
	return p.viewer != nil && m.Members[p.viewer.ID]
}

func (c *Core) has(scope string) bool {
	for _, s := range c.Granted {
		if s == scope {
			return true
		}
	}
	return false
}

func (c *Core) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+c.Token {
		c.fail(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var p principal
	if t := r.Header.Get(wire.ContextHeader); t != "" {
		ct, ok := c.tokens[t]
		if !ok {
			c.fail(w, http.StatusUnauthorized, "bad_context_token")
			return
		}
		p = principal{viewer: c.users[ct.user], matter: ct.matter}
	}
	who := "service"
	if !p.books() {
		who = "ctx"
	}
	c.Requests = append(c.Requests, r.Method+" "+r.URL.Path+" "+who)
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	parts := strings.Split(path, "/")
	modPrefix := "modules/" + quickbooks.ID + "/"
	switch {
	case r.Method == http.MethodPost && path == modPrefix+"context-tokens/verify":
		if p.books() {
			c.fail(w, http.StatusUnauthorized, "no_context_token")
			return
		}
		var mid *string
		if p.matter != "" {
			m := p.matter
			mid = &m
		}
		send(w, map[string]any{"viewer_user_id": p.viewer.ID, "roles": p.viewer.Roles, "module": quickbooks.ID, "matter_id": mid})
	case r.Method == http.MethodGet && path == modPrefix+"settings":
		keys := make([]string, 0, len(c.Settings))
		for k := range c.Settings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []map[string]string
		for _, k := range keys {
			out = append(out, map[string]string{"key": k, "value": c.Settings[k]})
		}
		send(w, out)
	case strings.HasPrefix(path, modPrefix+"secrets/"):
		c.secrets(w, r, strings.TrimPrefix(path, modPrefix+"secrets/"))
	case r.Method == http.MethodPost && path == "notifications":
		c.notify(w, r)
	case strings.HasPrefix(path, "books/"):
		c.booksAPI(w, r, p, parts)
	case r.Method != http.MethodGet:
		// The module holds no write scope on money records.
		c.fail(w, http.StatusForbidden, "scope_denied")
	default:
		c.read(w, r, p, parts)
	}
}

func (c *Core) secrets(w http.ResponseWriter, r *http.Request, name string) {
	if !c.has("module_secrets:write") {
		c.fail(w, http.StatusForbidden, "scope_denied")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var b struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Value == "" {
			c.fail(w, http.StatusBadRequest, "bad_secret")
			return
		}
		c.Secrets[name] = b.Value
		c.SecretWrites = append(c.SecretWrites, name)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		v, ok := c.Secrets[name]
		if !ok {
			c.fail(w, http.StatusNotFound, "not_found")
			return
		}
		send(w, map[string]string{"value": v})
	case http.MethodDelete:
		if _, ok := c.Secrets[name]; !ok {
			c.fail(w, http.StatusNotFound, "not_found")
			return
		}
		delete(c.Secrets, name)
		w.WriteHeader(http.StatusNoContent)
	default:
		c.fail(w, http.StatusMethodNotAllowed, "method")
	}
}

func (c *Core) notify(w http.ResponseWriter, r *http.Request) {
	if !c.has("notifications:write") {
		c.fail(w, http.StatusForbidden, "scope_denied")
		return
	}
	var b struct {
		Kind           string   `json:"kind"`
		Title          string   `json:"title"`
		Link           string   `json:"link"`
		RecipientRoles []string `json:"recipient_roles"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Kind != "sync_problem" {
		c.fail(w, http.StatusBadRequest, "unknown_kind")
		return
	}
	n := Notification{Kind: quickbooks.ID + "." + b.Kind, Title: b.Title, Link: b.Link, Roles: b.RecipientRoles}
	ids := make([]string, 0, len(c.users))
	for id := range c.users {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		u := c.users[id]
		if hasAny(u.Roles, b.RecipientRoles) {
			n.Recipients = append(n.Recipients, id)
			if u.EmailOn {
				n.Emailed = append(n.Emailed, id)
			}
		}
	}
	c.Notifications = append(c.Notifications, n)
	w.WriteHeader(http.StatusCreated)
	send(w, map[string]int{"recipients": len(n.Recipients)})
}

func hasAny(have, want []string) bool {
	for _, h := range have {
		for _, x := range want {
			if h == x {
				return true
			}
		}
	}
	return false
}

func (c *Core) booksAPI(w http.ResponseWriter, r *http.Request, p principal, parts []string) {
	if !p.books() || !c.has("books:read") {
		c.fail(w, http.StatusForbidden, "scope_denied")
		return
	}
	switch {
	case len(parts) == two && parts[one] == "profile":
		send(w, c.Profile)
	case len(parts) == three && parts[one] == "matters":
		m := c.matters[parts[two]]
		if m == nil || m.Walled {
			c.fail(w, http.StatusNotFound, "not_found")
			return
		}
		send(w, m.BooksMatter)
	case len(parts) == three && parts[one] == "contacts":
		ct := c.contacts[parts[two]]
		if ct == nil || c.contactHidden(ct.ID) {
			c.fail(w, http.StatusNotFound, "not_found")
			return
		}
		send(w, ct)
	default:
		c.fail(w, http.StatusNotFound, "not_found")
	}
}

// page writes one page of a list, with the cursor as an offset.
func page[T any](w http.ResponseWriter, r *http.Request, all []T) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= zero || limit > pageMax {
		limit = pageDefault
	}
	off, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	end := off + limit
	var next *string
	if end < len(all) {
		s := strconv.Itoa(end)
		next = &s
	} else {
		end = len(all)
	}
	if off > len(all) {
		off = len(all)
	}
	data := all[off:end]
	if data == nil {
		data = []T{}
	}
	send(w, map[string]any{"data": data, "page": map[string]any{"limit": limit, "next_cursor": next}})
}

func (c *Core) scope(p principal, s string) bool {
	if !c.has(s) {
		return false
	}
	if p.books() {
		return c.has("books:read")
	}
	return true
}

// include adds the module's sync columns for include=quickbooks.<table>.
func (c *Core) include(r *http.Request, table, id string) quickbooks.Included {
	want := r.URL.Query().Get("include")
	if want != quickbooks.ID+"."+table {
		return nil
	}
	key := map[string]string{"invoice_sync": "invoice_id", "payment_sync": "payment_id"}[table]
	rows, err := c.DB.Query(`SELECT state, synced_at, last_error FROM mod_quickbooks.`+table+` WHERE `+key+` = $1`, id)
	if err != nil {
		return quickbooks.Included{want: {}}
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var state, lastErr string
		var synced sql.NullInt64
		if rows.Scan(&state, &synced, &lastErr) == nil {
			row := map[string]any{"state": state, "last_error": lastErr, "synced_at": nil}
			if synced.Valid {
				row["synced_at"] = synced.Int64
			}
			out = append(out, row)
		}
	}
	return quickbooks.Included{want: out}
}

func (c *Core) read(w http.ResponseWriter, r *http.Request, p principal, parts []string) {
	q := r.URL.Query()
	matterOK := func(m string) bool { return c.sees(p, m) && (q.Get("matter_id") == "" || q.Get("matter_id") == m) }
	notFound := func() { c.fail(w, http.StatusNotFound, "not_found") }
	denied := func() { c.fail(w, http.StatusForbidden, "scope_denied") }
	switch parts[0] {
	case "invoices":
		if !c.scope(p, "invoices:read") {
			denied()
			return
		}
		if len(parts) == two {
			inv := c.invoices[parts[one]]
			if inv == nil || !c.sees(p, inv.MatterID) {
				notFound()
				return
			}
			out := *inv
			out.Included = c.include(r, "invoice_sync", inv.ID)
			send(w, out)
			return
		}
		var all []quickbooks.Invoice
		for _, id := range c.order {
			if inv := c.invoices[id]; inv != nil && matterOK(inv.MatterID) {
				out := *inv
				out.Included = c.include(r, "invoice_sync", inv.ID)
				all = append(all, out)
			}
		}
		page(w, r, all)
	case "payments":
		if !c.scope(p, "payments:read") {
			denied()
			return
		}
		if len(parts) == two {
			pm := c.payments[parts[one]]
			if pm == nil || !c.sees(p, pm.MatterID) {
				notFound()
				return
			}
			out := *pm
			out.Included = c.include(r, "payment_sync", pm.ID)
			send(w, out)
			return
		}
		var all []quickbooks.Payment
		for _, id := range c.order {
			if pm := c.payments[id]; pm != nil && matterOK(pm.MatterID) {
				out := *pm
				out.Included = c.include(r, "payment_sync", pm.ID)
				all = append(all, out)
			}
		}
		page(w, r, all)
	case "trust-ledger":
		if !c.scope(p, "trust:read") {
			denied()
			return
		}
		if len(parts) == two {
			e := c.entries[parts[one]]
			if e == nil || !c.sees(p, e.MatterID) {
				notFound()
				return
			}
			send(w, e)
			return
		}
		var all []quickbooks.TrustEntry
		for _, id := range c.order {
			if e := c.entries[id]; e != nil && matterOK(e.MatterID) {
				all = append(all, *e)
			}
		}
		page(w, r, all)
	case "trust-accounts":
		if !c.scope(p, "trust:read") {
			denied()
			return
		}
		var all []quickbooks.TrustAccount
		for _, a := range c.trustAccounts {
			all = append(all, *a)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
		page(w, r, all)
	case "trust":
		if len(parts) != two || parts[one] != "balances" || !c.scope(p, "trust:read") {
			denied()
			return
		}
		sums := map[string]int64{}
		for _, e := range c.entries {
			if c.sees(p, e.MatterID) {
				sums[e.TrustAccountID] += e.Signed()
			}
		}
		var out []quickbooks.TrustBalanceRow
		for id, n := range sums {
			out = append(out, quickbooks.TrustBalanceRow{Scope: "account", TrustAccountID: id, BalanceCents: n})
		}
		send(w, map[string]any{"data": out})
	case "expenses":
		if !c.scope(p, "expenses:read") {
			denied()
			return
		}
		if len(parts) == two {
			x := c.expenses[parts[one]]
			if x == nil || !c.sees(p, x.MatterID) {
				notFound()
				return
			}
			send(w, x)
			return
		}
		var all []quickbooks.Expense
		for _, id := range c.order {
			if x := c.expenses[id]; x != nil && matterOK(x.MatterID) {
				all = append(all, *x)
			}
		}
		page(w, r, all)
	case "matters":
		if p.books() || len(parts) != two || !c.has("matters:read") {
			denied()
			return
		}
		m := c.matters[parts[one]]
		if m == nil || !c.sees(p, m.ID) {
			notFound()
			return
		}
		send(w, quickbooks.Matter{ID: m.ID, Number: m.Number, Name: m.Name})
	default:
		notFound()
	}
}

// ---- install ----

// ErrDeclined is an install the admin cancelled.
var ErrDeclined = errors.New("install cancelled: a required scope was declined")

// Install applies the module's migrations to schema mod_quickbooks the way the core's
// runtime does (search_path set to the module schema, each version recorded) and grants
// the approved scopes. Declining books:read, or any required scope, cancels it.
func (c *Core) Install(ctx context.Context, requested, approved []string, migrations map[string][]byte) error {
	ok := map[string]bool{}
	for _, s := range approved {
		ok[s] = true
	}
	for _, s := range requested {
		if !ok[s] {
			return ErrDeclined
		}
	}
	conn, err := c.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE SCHEMA mod_quickbooks`); err != nil {
		return err
	}
	names := make([]string, 0, len(migrations))
	for n := range migrations {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO mod_quickbooks`); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, string(migrations[n])); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%s: %w", n, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO core.module_migrations (module, version) VALUES ($1, $2)`, quickbooks.ID, strings.TrimSuffix(n, ".sql")); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.Granted = append([]string(nil), approved...)
	c.mu.Unlock()
	return nil
}

// CoreSchema is the stand-in core schema: one id table per core record the module links
// to, and the migration record.
const CoreSchema = `
CREATE SCHEMA core;
CREATE TABLE core.users (id TEXT PRIMARY KEY);
CREATE TABLE core.contacts (id TEXT PRIMARY KEY);
CREATE TABLE core.matters (id TEXT PRIMARY KEY);
CREATE TABLE core.invoices (id TEXT PRIMARY KEY);
CREATE TABLE core.payments (id TEXT PRIMARY KEY);
CREATE TABLE core.trust_accounts (id TEXT PRIMARY KEY);
CREATE TABLE core.trust_ledger (id TEXT PRIMARY KEY);
CREATE TABLE core.expenses (id TEXT PRIMARY KEY);
CREATE TABLE core.module_migrations (module TEXT NOT NULL, version TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY (module, version));
`

// Reset drops and recreates the core and mod_quickbooks schemas in the test database
// (only those two, and only in the module's own database).
func Reset(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS mod_quickbooks CASCADE; DROP SCHEMA IF EXISTS core CASCADE;`+CoreSchema)
	return err
}
