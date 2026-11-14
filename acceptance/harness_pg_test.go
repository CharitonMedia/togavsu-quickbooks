// Package acceptance_test runs the QuickBooks module against the real composed core from
// the public module kit (pkg/modulekit/coretest) on Postgres. The module is built from the
// deployment's own token, webhook secret and database login, with the fake QBO injected.
//
// Criteria the core can't serve yet are written in full and skip at the step that needs
// the missing core item, naming it, so the suite turns green item by item as module-core
// lands. The suite finds what is missing itself: the install checks and scope catalog of
// the pinned core say which package parts it refuses, and probes of the core's API say
// which routes it lacks.
package acceptance_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	quickbooks "github.com/CharitonMedia/togavsu-quickbooks"
	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
	"github.com/CharitonMedia/togavsu-quickbooks/qbo/fake"
	"github.com/CharitonMedia/togavsu/pkg/modulekit"
	"github.com/CharitonMedia/togavsu/pkg/modulekit/coretest"
)

const (
	realmA = "9130350000000001"
	realmB = "9130350000000002"
	id     = quickbooks.ID
)

// Core items the suite checks for. billingTables is a core gap no CORE item names yet:
// the core keeps invoices, payments and expenses as documents in core.billing_records,
// so core.invoices, core.payments and core.expenses stay empty and the module's foreign
// keys to them can't be met.
const (
	core01        = "CORE-01"
	core02        = "CORE-02"
	core03        = "CORE-03"
	core06        = "CORE-06"
	core08        = "CORE-08"
	core09        = "CORE-09"
	core22        = "CORE-22"
	billingTables = "core billing tables"
	scopeReasons  = "scope reasons on the Modules tab"
)

var why = map[string]string{
	core01:        "service calls without a context token",
	core02:        "books:read and the books reader",
	core03:        "manifest notifications and createNotification",
	core06:        "module_secrets:write and the module secret store",
	core08:        "sidebar section, icon and order",
	core09:        "frames that may open a popup",
	core22:        "trust_account as a linkable core record",
	billingTables: "invoices, payments and expenses as rows of core.invoices, core.payments and core.expenses (no CORE item yet)",
	scopeReasons:  "the admin's permission form showing each scope's manifest reason (no CORE item yet)",
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func (c *clock) Set(t time.Time)         { c.mu.Lock(); c.t = t; c.mu.Unlock() }

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *logBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// suite is one composed core with the module installed and approved.
type suite struct {
	t       *testing.T
	ctx     context.Context
	c       *coretest.Core
	qb      *fake.Fake
	clock   *clock
	logs    *logBuffer
	missing map[string]bool
	files   map[string][]byte // the package as installed
	signed  []byte
	db      *sql.DB // the module's own login

	mu  sync.Mutex
	srv *quickbooks.Server

	trust        string            // the trust account
	clients      map[string]string // matter to client contact
	clientSecret string
}

// newSuite installs the module on a fresh core and approves it; QuickBooks is not
// connected.
func newSuite(t *testing.T) *suite {
	t.Helper()
	s := &suite{t: t, ctx: context.Background(), c: coretest.New(t), qb: fake.New(), logs: &logBuffer{}, missing: map[string]bool{}, clients: map[string]string{},
		clock: &clock{t: time.Now().UTC()}}
	s.qb.SetClock(s.clock.Now)
	s.qb.Seed(realmA, fake.SampleChart())
	s.qb.Seed(realmB, fake.SampleChart())
	s.c.Serve(id, func(m coretest.Module) http.Handler {
		srv := quickbooks.New(quickbooks.Config{Core: m.CoreURL, Token: m.Token, WebhookSecret: m.WebhookSecret, DB: m.DB,
			QBO: s.qb, Now: s.clock.Now, Log: s.logs})
		s.mu.Lock()
		s.srv = srv
		s.mu.Unlock()
		return srv.Handler()
	})
	s.files = s.installable()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.signed, err = quickbooks.SignFile(priv, s.files); err != nil {
		t.Fatal(err)
	}
	s.c.TrustKey("QuickBooks Sync test key", pub)
	s.c.Install(s.signed)
	s.c.ApproveAll(id)
	s.db = s.c.ModuleDB(id)
	s.probe()
	s.clientSecret = "synthetic-secret-" + strings.ReplaceAll(t.Name(), "/", "-")
	return s
}

// installable is the package as the pinned core accepts it. Each part the core refuses
// for a missing CORE item comes out and the item is marked missing; any other refusal
// fails the test.
func (s *suite) installable() map[string][]byte {
	s.t.Helper()
	files, err := quickbooks.Files()
	if err != nil {
		s.t.Fatal(err)
	}
	strip := func(name, pattern, item string) {
		before := string(files[name])
		after := regexp.MustCompile(pattern).ReplaceAllString(before, "")
		if after == before {
			s.t.Fatalf("the core refused %s for %s and stripping changed nothing", name, item)
		}
		files[name] = []byte(after)
		s.missing[item] = true
	}
	const manifest = "togavsu-module.yaml"
	// Scopes the core's catalog doesn't know make approval fail, so probe them first.
	for scope, item := range map[string]string{"books:read": core02, "module_secrets:write": core06} {
		r := s.c.Admin.Do("POST", "/api/v1/api-tokens", map[string]any{"name": "scope probe", "owner_user_id": s.c.Admin.ID, "scopes": []string{scope}})
		if strings.Contains(r.Body, "unknown scope") {
			strip(manifest, `(?m)^  - scope: `+regexp.QuoteMeta(scope)+`\n    reason: .*\n`, item)
		}
	}
	for tries := 0; ; tries++ {
		_, _, err := modulekit.Check(files)
		if err == nil {
			return files
		}
		if tries > 5 {
			s.t.Fatalf("the core still refuses the package: %v", err)
		}
		msg := err.Error()
		switch {
		case strings.Contains(msg, `unknown field "notifications"`):
			strip(manifest, `(?m)^notifications:\n(?:  .*\n)+`, core03)
		case regexp.MustCompile(`unknown field "(section|icon|order|subgroup|badge)"`).MatchString(msg):
			strip(manifest, `(?m)^    (section|icon|order|subgroup|badge): .*\n`, core08)
		case strings.Contains(msg, "core.trust_accounts is not a core record"):
			strip("migrations/0001_connection.sql", ` REFERENCES core\.trust_accounts \(id\) ON DELETE RESTRICT`, core22)
		default:
			s.t.Fatalf("the core refuses the package: %v (%s)", err, modulekit.Code(err))
		}
	}
}

// probe marks the API items the running core lacks.
func (s *suite) probe() {
	mod := s.c.ModuleClient(id)
	if r := mod.Do("GET", "/api/v1/invoices", nil); r.Code != http.StatusOK {
		s.missing[core01] = true
	}
	if r := mod.Do("GET", "/api/v1/books/profile", nil); r.Code != http.StatusOK {
		s.missing[core02] = true
	}
	// A secret store answers a missing name with its JSON not_found; no route answers text.
	if r := mod.Do("GET", "/api/v1/modules/"+id+"/secrets/qbo/probe", nil); r.JSON == nil {
		s.missing[core06] = true
	}
	if page := s.c.Admin.Do("GET", "/modules/"+id+"/tabs/books", nil); !strings.Contains(page.Body, "allow-popups") {
		s.missing[core09] = true
	}
}

// needs skips the test when any of items is missing from the core, naming them.
func (s *suite) needs(items ...string) {
	s.t.Helper()
	var miss []string
	for _, it := range items {
		if s.missing[it] {
			miss = append(miss, it+" ("+why[it]+")")
		}
	}
	if len(miss) > 0 {
		sort.Strings(miss)
		s.t.Skip("needs " + strings.Join(miss, ", "))
	}
}

// pushes is what every push needs from the core: service calls, the books reader and the
// secret store that holds the QuickBooks sign-in.
var pushes = []string{core01, core02, core06}

// newConnected is a suite with the settings mapped and QuickBooks connected to realm A.
func newConnected(t *testing.T) *suite {
	t.Helper()
	s := newSuite(t)
	s.needs(pushes...)
	s.configure(nil)
	if r := s.connect(realmA); r.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", r.Code, r.Body)
	}
	return s
}

// configure saves the mapping settings, with extra on top.
func (s *suite) configure(extra map[string]string) {
	s.t.Helper()
	set := map[string]string{"qbo_client_id": "synthetic-client", "account_operating_bank": "Operating Checking", "account_trust_bank": "IOLTA Trust",
		"sync_start_date": s.clock.Now().AddDate(0, 0, -30).Format(time.DateOnly)}
	for k, v := range extra {
		set[k] = v
	}
	s.setting(set)
}

func (s *suite) setting(set map[string]string) {
	s.t.Helper()
	s.c.Admin.Must(http.StatusOK, "PUT", "/api/v1/modules/"+id+"/settings", set)
}

func (s *suite) server() *quickbooks.Server {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		s.t.Fatal("the module was never deployed")
	}
	return s.srv
}

// ctxFor is a context token for who, on matter or none.
func (s *suite) ctxFor(who, matter string) string { return s.c.ContextFor(who, id, matter) }

// get calls a module page or endpoint through the core's /m/ route as who.
func (s *suite) get(who, path, tok string) coretest.Resp {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return s.c.As(who).Do("GET", "/m/"+id+path+sep+"context="+url.QueryEscape(tok), nil)
}

// act posts an agent action as JSON through the core's /m/ route as who.
func (s *suite) act(who, action, tok string, body map[string]string) coretest.Resp {
	if body == nil {
		body = map[string]string{}
	}
	return s.c.As(who).Do("POST", "/m/"+id+"/api/"+action+"?context="+url.QueryEscape(tok), body)
}

func (s *suite) mustAct(who, action string, body map[string]string) {
	s.t.Helper()
	if r := s.act(who, action, s.ctxFor(who, ""), body); r.Code != http.StatusOK {
		s.t.Fatalf("%s as %s: %d %s", action, who, r.Code, r.Body)
	}
}

func decode[T any](t *testing.T, r coretest.Resp) T {
	t.Helper()
	var v T
	if r.Code != http.StatusOK {
		t.Fatalf("status %d: %s", r.Code, r.Body)
	}
	if err := json.Unmarshal([]byte(r.Body), &v); err != nil {
		t.Fatalf("%v: %s", err, r.Body)
	}
	return v
}

// connect stores the client secret and runs the OAuth flow as the admin.
func (s *suite) connect(realm string) coretest.Resp {
	s.t.Helper()
	s.mustAct(coretest.Admin, "client-secret", map[string]string{"client_secret": s.clientSecret})
	setup := decode[quickbooks.SetupView](s.t, s.get(coretest.Admin, "/api/setup", s.ctxFor(coretest.Admin, "")))
	if setup.ConnectURL == "" {
		s.t.Fatal("no connect URL for the admin")
	}
	browser := s.c.NewClient()
	start := browser.Do("GET", "/m/"+id+"/"+setup.ConnectURL, nil)
	if start.Code != http.StatusFound {
		s.t.Fatalf("start: %d %s", start.Code, start.Body)
	}
	loc, err := url.Parse(start.Loc)
	if err != nil {
		s.t.Fatal(err)
	}
	q := url.Values{"state": {loc.Query().Get("state")}, "code": {s.qb.IssueCode(realm)}, "realmId": {realm}}
	return browser.Do("GET", "/m/"+id+"/oauth/callback?"+q.Encode(), nil)
}

// tick runs one worker pass.
func (s *suite) tick() {
	s.t.Helper()
	if err := s.server().Tick(s.ctx); err != nil {
		s.t.Fatalf("tick: %v", err)
	}
}

// deliver posts the core's pending events to the module, then runs the worker.
func (s *suite) deliver() {
	s.t.Helper()
	s.c.DeliverEvents()
	s.tick()
}

func (s *suite) syncNow() { s.t.Helper(); s.mustAct(coretest.Admin, "sync-now", nil) }

// --- synthetic money through the core API ---

// timeEntry logs billable time at a rate on matter as the attorney.
func (s *suite) timeEntry(matter string, increments int, rate int64) string {
	s.t.Helper()
	r := s.c.Attorney.Must(http.StatusCreated, "POST", "/api/v1/time-entries", map[string]any{"matter_id": matter,
		"worked_on": s.clock.Now().Format(time.DateOnly), "increments": increments, "description": "Synthetic drafting",
		"activity_type_id": s.c.Activity, "billable": true, "rate_cents": rate})
	return str(s.t, r.JSON, "id")
}

// expense records an expense on matter.
func (s *suite) expense(matter, description string, cents int64, billable bool) string {
	s.t.Helper()
	r := s.c.Admin.Must(http.StatusCreated, "POST", "/api/v1/expenses", map[string]any{"matter_id": matter,
		"incurred_on": s.clock.Now().Format(time.DateOnly), "description": description, "amount_cents": cents, "billable": billable})
	return str(s.t, r.JSON, "id")
}

// draft drafts the matter's unbilled time and expenses into one invoice.
func (s *suite) draft(matter string) string {
	s.t.Helper()
	r := s.c.Attorney.Must(http.StatusCreated, "POST", "/api/v1/invoice-drafts", map[string]any{"matter_id": matter})
	data, _ := r.JSON["data"].([]any)
	if len(data) != 1 {
		s.t.Fatalf("want one draft: %s", r.Body)
	}
	inv := str(s.t, data[0].(map[string]any), "id")
	s.needsRow("invoices", inv)
	return inv
}

// send approves and sends a draft.
func (s *suite) send(inv string) {
	s.t.Helper()
	s.c.Attorney.Must(http.StatusOK, "POST", "/api/v1/invoices/"+inv+"/approve", nil)
	s.c.Attorney.Must(http.StatusOK, "POST", "/api/v1/invoices/"+inv+"/send", map[string]any{"deliver": "file"})
}

// sentInvoice sends an invoice of one time line worth increments at rate.
func (s *suite) sentInvoice(matter string, increments int, rate int64) string {
	s.t.Helper()
	s.timeEntry(matter, increments, rate)
	inv := s.draft(matter)
	s.send(inv)
	return inv
}

// matter opens a matter whose client is "Client <last word of name>" and returns the
// matter and client ids.
func (s *suite) matter(name string) (matter, client string) {
	s.t.Helper()
	matter, client = s.c.Matter(name, "", nil)
	s.clients[matter] = client
	return matter, client
}

// clientOf is the matter's client contact.
func (s *suite) clientOf(matter string) string {
	s.t.Helper()
	if matter == s.c.Open {
		return s.c.ClientContact
	}
	c, ok := s.clients[matter]
	if !ok {
		s.t.Fatalf("no client known for matter %s", matter)
	}
	return c
}

// trustAccount opens the firm's trust account once.
func (s *suite) trustAccount() string {
	s.t.Helper()
	if s.trust == "" {
		s.trust = str(s.t, s.c.Admin.Must(http.StatusCreated, "POST", "/api/v1/trust-accounts", map[string]any{
			"bank": "Synthetic Bank", "name": "IOLTA Trust", "iolta": true, "last_four": "4242"}).JSON, "id")
	}
	return s.trust
}

// postTrust posts a trust ledger entry on matter.
func (s *suite) postTrust(matter, typ string, cents int64, description string) string {
	s.t.Helper()
	r := s.c.Attorney.Must(http.StatusCreated, "POST", "/api/v1/trust-ledger", map[string]any{"trust_account_id": s.trustAccount(),
		"client_id": s.clientOf(matter), "matter_id": matter, "entry_type": typ, "amount_cents": cents, "method": "check", "description": description})
	return str(s.t, r.JSON, "id")
}

// reverseTrust reverses a trust entry.
func (s *suite) reverseTrust(entry string) string {
	s.t.Helper()
	r := s.c.Attorney.Must(http.StatusCreated, "POST", "/api/v1/trust-ledger/"+entry+"/reversals", map[string]any{"reason": "keyed twice"})
	return str(s.t, r.JSON, "id")
}

// payment records a payment on matter, applied to inv when it is set.
func (s *suite) payment(matter, inv, destination, method string, cents int64) string {
	s.t.Helper()
	body := map[string]any{"contact_id": s.clientOf(matter), "matter_id": matter, "amount_cents": cents, "destination": destination,
		"method": method, "reference_number": "1001"}
	if inv != "" {
		body["invoice_id"] = inv
	}
	if destination == "trust" {
		body["trust_account_id"] = s.trustAccount()
	}
	p := str(s.t, s.c.Admin.Must(http.StatusCreated, "POST", "/api/v1/payments", body).JSON, "id")
	s.needsRow("payments", p)
	return p
}

func (s *suite) reversePayment(p string) {
	s.t.Helper()
	s.c.Admin.Must(http.StatusOK, "POST", "/api/v1/payments/"+p+"/reverse", map[string]any{"reason": "bounced"})
}

func (s *suite) writeOff(inv string, cents int64) {
	s.t.Helper()
	s.c.Admin.Must(http.StatusOK, "POST", "/api/v1/invoices/"+inv+"/write-off", map[string]any{"amount_cents": cents, "reason": "hardship"})
}

func (s *suite) void(inv string) {
	s.t.Helper()
	s.c.Attorney.Must(http.StatusOK, "POST", "/api/v1/invoices/"+inv+"/void", map[string]any{"reason": "issued in error"})
}

// applyTrust applies trust to a sent invoice and returns the transfer entry and the
// operating payment it made.
func (s *suite) applyTrust(inv string, cents int64) (entry, pay string) {
	s.t.Helper()
	before := s.ids("/api/v1/trust-ledger")
	beforePay := s.ids("/api/v1/payments")
	s.c.Admin.Must(http.StatusOK, "POST", "/api/v1/invoices/"+inv+"/apply-trust", map[string]any{"client_id": s.clientOf(s.matterOf(inv)),
		"trust_account_id": s.trustAccount(), "amount_cents": cents})
	return s.newID("/api/v1/trust-ledger", before), s.newID("/api/v1/payments", beforePay)
}

// applyFlatFee applies a flat fee held in trust and returns the entry and payment.
func (s *suite) applyFlatFee(matter string, cents int64) (entry, pay string) {
	s.t.Helper()
	before := s.ids("/api/v1/trust-ledger")
	beforePay := s.ids("/api/v1/payments")
	s.c.Admin.Must(http.StatusOK, "POST", "/api/v1/billing/flat-fees/"+matter+"/apply", map[string]any{"client_id": s.clientOf(matter),
		"trust_account_id": s.trustAccount(), "amount_cents": cents})
	return s.newID("/api/v1/trust-ledger", before), s.newID("/api/v1/payments", beforePay)
}

func (s *suite) matterOf(inv string) string {
	s.t.Helper()
	return str(s.t, s.c.Admin.Must(http.StatusOK, "GET", "/api/v1/invoices/"+inv, nil).JSON, "matter_id")
}

func (s *suite) invoice(inv string) map[string]any {
	s.t.Helper()
	return s.c.Admin.Must(http.StatusOK, "GET", "/api/v1/invoices/"+inv, nil).JSON
}

func (s *suite) ids(list string) map[string]bool {
	s.t.Helper()
	out := map[string]bool{}
	data, _ := s.c.Admin.Must(http.StatusOK, "GET", list+"?limit=200", nil).JSON["data"].([]any)
	for _, d := range data {
		if m, ok := d.(map[string]any); ok {
			out[m["id"].(string)] = true
		}
	}
	return out
}

func (s *suite) newID(list string, before map[string]bool) string {
	s.t.Helper()
	for k := range s.ids(list) {
		if !before[k] {
			return k
		}
	}
	s.t.Fatalf("no new record in %s", list)
	return ""
}

// needsRow skips the test when the core keeps record id out of its relational table, so
// no module foreign key to it can hold.
func (s *suite) needsRow(table, recordID string) {
	s.t.Helper()
	if s.c.Count(`SELECT count(*) FROM core.`+table+` WHERE id = $1`, recordID) == 0 {
		s.missing[billingTables] = true
	}
	s.needs(billingTables)
}

// --- the module's tables ---

type rowState struct {
	State, SkipReason, WaitingOn, QBOID, Code, Error, RequestID string
	Attempts                                                    int
	NextAttemptAt                                               int64
}

func (s *suite) row(table, key, recordID string) (rowState, bool) {
	s.t.Helper()
	var r rowState
	err := s.db.QueryRow(`SELECT state, skip_reason, waiting_on, qbo_id, last_error_code, last_error, request_id, attempts, next_attempt_at FROM `+table+` WHERE `+key+` = $1`, recordID).
		Scan(&r.State, &r.SkipReason, &r.WaitingOn, &r.QBOID, &r.Code, &r.Error, &r.RequestID, &r.Attempts, &r.NextAttemptAt)
	if err == sql.ErrNoRows {
		return r, false
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return r, true
}

func (s *suite) must(r rowState, ok bool) rowState {
	s.t.Helper()
	if !ok {
		s.t.Fatal("no sync row")
	}
	return r
}

func (s *suite) invoiceRow(k string) rowState { return s.must(s.row("invoice_sync", "invoice_id", k)) }
func (s *suite) paymentRow(k string) rowState { return s.must(s.row("payment_sync", "payment_id", k)) }
func (s *suite) trustRow(k string) rowState {
	return s.must(s.row("trust_entry_sync", "trust_entry_id", k))
}
func (s *suite) expenseRow(k string) rowState { return s.must(s.row("expense_sync", "expense_id", k)) }

func (s *suite) customerID(contact string) string {
	s.t.Helper()
	var v string
	if err := s.db.QueryRow(`SELECT qbo_id FROM customer_sync WHERE contact_id = $1`, contact).Scan(&v); err != nil {
		s.t.Fatal(err)
	}
	return v
}

func (s *suite) count(q string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// dump renders every row of every module table as text.
func (s *suite) dump() string {
	s.t.Helper()
	rows, err := s.db.Query(`SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema()`)
	if err != nil {
		s.t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	var b strings.Builder
	for _, tb := range tables {
		rs, err := s.db.Query(`SELECT t::text FROM ` + tb + ` t`)
		if err != nil {
			s.t.Fatal(err)
		}
		for rs.Next() {
			var v string
			_ = rs.Scan(&v)
			b.WriteString(v + "\n")
		}
		rs.Close()
	}
	return b.String()
}

// notifications lists who's notifications of kind, by role.
func (s *suite) notifications(who, kind string) []map[string]any {
	s.t.Helper()
	var out []map[string]any
	data, _ := s.c.As(who).Must(http.StatusOK, "GET", "/api/v1/notifications", nil).JSON["data"].([]any)
	for _, d := range data {
		if m, ok := d.(map[string]any); ok && m["kind"] == kind {
			out = append(out, m)
		}
	}
	return out
}

// --- QBO helpers ---

func (s *suite) entities(k qbo.Kind) []qbo.Entity { return s.qb.Entities(realmA, k) }

func journal(t *testing.T, e qbo.Entity) *qbo.JournalEntry {
	t.Helper()
	je, ok := e.(*qbo.JournalEntry)
	if !ok {
		t.Fatalf("not a journal entry: %T", e)
	}
	return je
}

// lineSet renders a journal entry's lines as "Debit 36 150000" strings.
func lineSet(je *qbo.JournalEntry) []string {
	var out []string
	for _, l := range je.Line {
		d := l.JournalEntryLineDetail
		v := d.PostingType + " " + d.AccountRef.Value + " " + itoa(int64(l.Amount))
		if d.Entity != nil {
			v += " customer " + d.Entity.EntityRef.Value
		}
		out = append(out, v)
	}
	return out
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func str(t *testing.T, m map[string]any, k string) string {
	t.Helper()
	v, ok := m[k].(string)
	if !ok || v == "" {
		t.Fatalf("no %s in %v", k, m)
	}
	return v
}

func jsonInto(body string, v any) error { return json.Unmarshal([]byte(body), v) }

func cents(m map[string]any, k string) int64 {
	f, _ := m[k].(float64)
	return int64(f)
}

func unavailable() *qbo.Fault {
	return &qbo.Fault{HTTPStatus: 503, Code: "503", Message: "unavailable", Retryable: true}
}
