// Package fake is an in-memory QuickBooks Online for tests. It implements qbo.Provider,
// qbo.Auth and qbo.Books, enforces the rules the module depends on (unique display names,
// SyncToken concurrency, balanced journal entries on known accounts, void semantics,
// request id replay, expiring access tokens and rotating refresh tokens) and records a
// change log for the change data capture read. It never reaches the network.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
)

// SandboxBase and ProductionBase are the addresses BaseURL reports, matching Intuit's.
const (
	SandboxBase    = "https://sandbox-quickbooks.api.intuit.com"
	ProductionBase = "https://quickbooks.api.intuit.com"
)

// Call is one recorded adapter call.
type Call struct {
	Realm     string
	Op        string // create, update, void, delete, read, query, changes, company, create_account, create_item
	Kind      qbo.Kind
	ID        string
	RequestID string
	Replayed  bool
}

// IsWrite reports whether the call changed the books.
func (c Call) IsWrite() bool {
	switch c.Op {
	case "create", "update", "void", "delete", "create_account", "create_item":
		return true
	}
	return false
}

type stored struct {
	kind      qbo.Kind
	id        string
	syncToken int
	body      []byte // the entity JSON
	voided    bool
	deleted   bool
}

type replay struct {
	ref qbo.Ref
	err error
}

type company struct {
	name     string
	accounts []qbo.Account
	items    []qbo.Item
	methods  []qbo.PaymentMethod
	others   map[string]string // display name -> Vendor or Employee
	entities map[qbo.Kind]map[string]*stored
	nextID   int
	replays  map[string]replay
	changes  []qbo.Change
	fixed    bool
	envSeen  string
}

type token struct {
	realm   string
	expires time.Time
	revoked bool
}

// Fake is one in-memory QBO with any number of companies.
type Fake struct {
	mu        sync.Mutex
	now       func() time.Time
	companies map[string]*company
	codes     map[string]string // code -> realm
	access    map[string]*token
	refresh   map[string]*token
	n         int
	calls     []Call
	failNext  map[qbo.Kind][]*qbo.Fault
	timeout   map[qbo.Kind]int
	throttle  int
	revokeRef bool
	// Exchanged records the client ids and redirect URIs used at the token endpoint.
	Exchanged []string
}

// New returns an empty fake on the wall clock.
func New() *Fake {
	return &Fake{now: time.Now, companies: map[string]*company{}, codes: map[string]string{}, access: map[string]*token{},
		refresh: map[string]*token{}, failNext: map[qbo.Kind][]*qbo.Fault{}, timeout: map[qbo.Kind]int{}}
}

// SetClock sets the fake's clock (tests share one with the module).
func (f *Fake) SetClock(now func() time.Time) { f.mu.Lock(); f.now = now; f.mu.Unlock() }

// SeedData is a synthetic company.
type SeedData struct {
	Name     string
	Accounts []qbo.Account
	Items    []qbo.Item
	Methods  []qbo.PaymentMethod
	Vendors  []string
}

// SampleChart is a synthetic chart of accounts for "Sample Law Co": banks, Undeposited
// Funds, AR, the trust liability, income accounts, the client costs asset, a credit
// card, the default items and the usual payment methods.
func SampleChart() SeedData {
	return SeedData{
		Name: "Sample Law Co",
		Accounts: []qbo.Account{
			{ID: "35", Name: "Operating Checking", AccountType: "Bank", AccountSubType: "Checking", Active: true},
			{ID: "36", Name: "IOLTA Trust", AccountType: "Bank", AccountSubType: "Checking", Active: true},
			{ID: "37", Name: "Second Trust", AccountType: "Bank", AccountSubType: "Checking", Active: true},
			{ID: "4", Name: "Undeposited Funds", AccountType: "Other Current Asset", AccountSubType: "UndepositedFunds", Active: true},
			{ID: "84", Name: "Accounts Receivable (A/R)", AccountType: "Accounts Receivable", AccountSubType: "AccountsReceivable", Active: true},
			{ID: "90", Name: "Client Trust Liability", AccountType: "Other Current Liability", AccountSubType: "TrustAccountsLiabilities", Active: true},
			{ID: "79", Name: "Legal Fee Income", AccountType: "Income", AccountSubType: "ServiceFeeIncome", Active: true},
			{ID: "80", Name: "Cost Recovery Income", AccountType: "Income", AccountSubType: "OtherPrimaryIncome", Active: true},
			{ID: "91", Name: "Client Costs Advanced", AccountType: "Other Current Asset", AccountSubType: "OtherCurrentAssets", Active: true},
			{ID: "41", Name: "Firm Card", AccountType: "Credit Card", AccountSubType: "CreditCard", Active: true},
		},
		Items: []qbo.Item{
			{ID: "1", Name: "Legal Services", Type: "Service", IncomeAccountRef: &qbo.RefVal{Value: "79"}, Active: true},
			{ID: "2", Name: "Flat Fee", Type: "Service", IncomeAccountRef: &qbo.RefVal{Value: "79"}, Active: true},
			{ID: "3", Name: "Reimbursed Costs", Type: "Service", IncomeAccountRef: &qbo.RefVal{Value: "80"}, Active: true},
			{ID: "4", Name: "Invoice Adjustment", Type: "Service", IncomeAccountRef: &qbo.RefVal{Value: "79"}, Active: true},
			{ID: "5", Name: "Late Interest", Type: "Service", IncomeAccountRef: &qbo.RefVal{Value: "79"}, Active: true},
			{ID: "6", Name: "Write-Off", Type: "Service", IncomeAccountRef: &qbo.RefVal{Value: "79"}, Active: true},
		},
		Methods: []qbo.PaymentMethod{{ID: "1", Name: "Cash"}, {ID: "2", Name: "Check"}, {ID: "3", Name: "Credit Card"}, {ID: "4", Name: "ACH"}},
	}
}

// Seed adds (or replaces) a company.
func (f *Fake) Seed(realm string, d SeedData) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &company{name: d.Name, others: map[string]string{}, entities: map[qbo.Kind]map[string]*stored{}, nextID: firstEntityID, replays: map[string]replay{}}
	for _, a := range d.Accounts {
		if a.FullyQualifiedName == "" {
			a.FullyQualifiedName = a.Name
		}
		c.accounts = append(c.accounts, a)
	}
	for _, i := range d.Items {
		if i.FullyQualifiedName == "" {
			i.FullyQualifiedName = i.Name
		}
		c.items = append(c.items, i)
	}
	c.methods = append(c.methods, d.Methods...)
	for _, v := range d.Vendors {
		c.others[v] = "Vendor"
	}
	f.companies[realm] = c
}

// AddVendor takes a display name for a vendor, so a customer can't use it.
func (f *Fake) AddVendor(realm, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.companies[realm].others[name] = "Vendor"
}

// AddCustomer creates a customer as if the accountant made it in QuickBooks.
func (f *Fake) AddCustomer(realm string, c qbo.Customer) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	co := f.companies[realm]
	id := co.newID()
	c.ID, c.SyncToken = id, "0"
	co.put(&stored{kind: qbo.KindCustomer, id: id, body: mustJSON(&c)}, f.now())
	return id
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func (c *company) newID() string { c.nextID++; return strconv.Itoa(c.nextID) }

func (c *company) put(s *stored, at time.Time) {
	m := c.entities[s.kind]
	if m == nil {
		m = map[string]*stored{}
		c.entities[s.kind] = m
	}
	m[s.id] = s
	c.changes = append(c.changes, qbo.Change{Kind: s.kind, ID: s.id, SyncToken: strconv.Itoa(s.syncToken), Deleted: s.deleted, UpdatedAt: at})
}

func (c *company) get(k qbo.Kind, id string) *stored {
	if m := c.entities[k]; m != nil {
		if s := m[id]; s != nil && !s.deleted {
			return s
		}
	}
	return nil
}

func (s *stored) ref() qbo.Ref { return qbo.Ref{ID: s.id, SyncToken: strconv.Itoa(s.syncToken)} }

func (s *stored) entity() qbo.Entity {
	e, _ := qbo.New(s.kind)
	_ = json.Unmarshal(s.body, e)
	e.SetRef(s.ref())
	return e
}

// ---- hooks ----

// FailNext makes the next write of kind fail with fault (calls queue).
func (f *Fake) FailNext(k qbo.Kind, fault *qbo.Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[k] = append(f.failNext[k], fault)
}

// Throttle answers the next n calls with 429.
func (f *Fake) Throttle(n int) { f.mu.Lock(); f.throttle = n; f.mu.Unlock() }

// Timeout makes the next write of kind apply and then fail as a lost answer.
func (f *Fake) Timeout(k qbo.Kind) { f.mu.Lock(); f.timeout[k]++; f.mu.Unlock() }

// RevokeRefresh makes the next refresh answer invalid_grant.
func (f *Fake) RevokeRefresh() { f.mu.Lock(); f.revokeRef = true; f.mu.Unlock() }

// EditAsAccountant changes an entity's private note as a person would in QuickBooks.
func (f *Fake) EditAsAccountant(realm string, k qbo.Kind, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.companies[realm]
	s := c.get(k, id)
	var m map[string]any
	_ = json.Unmarshal(s.body, &m)
	m["PrivateNote"] = fmt.Sprint(m["PrivateNote"]) + " (edited in QuickBooks)"
	if lines, ok := m["Line"].([]any); ok && len(lines) > 0 && k == qbo.KindInvoice {
		l := lines[0].(map[string]any)
		l["Description"] = "Changed by the accountant"
	}
	s.body = mustJSON(m)
	s.syncToken++
	c.put(s, f.now())
}

// AlterJournalEntry sets every line of a journal entry to cents (it stays balanced).
func (f *Fake) AlterJournalEntry(realm, id string, cents int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.companies[realm]
	s := c.get(qbo.KindJournalEntry, id)
	var je qbo.JournalEntry
	_ = json.Unmarshal(s.body, &je)
	for i := range je.Line {
		je.Line[i].Amount = qbo.Money(cents)
	}
	s.body = mustJSON(&je)
	s.syncToken++
	c.put(s, f.now())
}

// DeleteAsAccountant deletes an entity as a person would in QuickBooks.
func (f *Fake) DeleteAsAccountant(realm string, k qbo.Kind, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.companies[realm]
	s := c.get(k, id)
	s.deleted = true
	s.syncToken++
	c.put(s, f.now())
}

// Calls returns the recorded calls.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Writes returns the recorded write calls that reached the books (not replays).
func (f *Fake) Writes() []Call {
	var out []Call
	for _, c := range f.Calls() {
		if c.IsWrite() && !c.Replayed {
			out = append(out, c)
		}
	}
	return out
}

// ResetCalls forgets the recorded calls.
func (f *Fake) ResetCalls() { f.mu.Lock(); f.calls = nil; f.mu.Unlock() }

// Entities lists the live entities of kind.
func (f *Fake) Entities(realm string, k qbo.Kind) []qbo.Entity {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.companies[realm]
	var ids []string
	for id, s := range c.entities[k] {
		if !s.deleted {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var out []qbo.Entity
	for _, id := range ids {
		out = append(out, c.entities[k][id].entity())
	}
	return out
}

// Get returns one live entity, or nil.
func (f *Fake) Get(realm string, k qbo.Kind, id string) qbo.Entity {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.companies[realm].get(k, id); s != nil {
		return f.decorate(f.companies[realm], s)
	}
	return nil
}

// Voided reports whether an entity is voided.
func (f *Fake) Voided(realm string, k qbo.Kind, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.companies[realm].get(k, id)
	return s != nil && s.voided
}

// IssueCode is the user approving the app for realm: it returns the code Intuit would
// send to the redirect URI.
func (f *Fake) IssueCode(realm string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	code := "fake-code-" + strconv.Itoa(f.n)
	f.codes[code] = realm
	return code
}

// AccessValid reports whether an access token works now.
func (f *Fake) AccessValid(tok string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.access[tok]
	return t != nil && !t.revoked && f.now().Before(t.expires)
}

// RefreshValid reports whether a refresh token works now.
func (f *Fake) RefreshValid(tok string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.refresh[tok]
	return t != nil && !t.revoked
}

// ---- Provider and Auth ----

// BaseURL reports Intuit's base address for the environment.
func (f *Fake) BaseURL(environment string) string {
	if environment == "production" {
		return ProductionBase
	}
	return SandboxBase
}

type auth struct {
	f       *Fake
	env, id string
	secret  string
}

// Auth returns the fake OAuth client.
func (f *Fake) Auth(environment, clientID, clientSecret string) qbo.Auth {
	return &auth{f: f, env: environment, id: clientID, secret: clientSecret}
}

func (a *auth) AuthCodeURL(state, redirectURI string) string {
	return "https://appcenter.intuit.com/connect/oauth2?client_id=" + a.id + "&response_type=code&scope=" + qbo.Scope + "&redirect_uri=" + redirectURI + "&state=" + state
}

func (f *Fake) issue(realm string) qbo.Tokens {
	f.n++
	now := f.now()
	acc := "fake-access-" + strconv.Itoa(f.n)
	ref := "fake-refresh-" + strconv.Itoa(f.n)
	f.access[acc] = &token{realm: realm, expires: now.Add(AccessLifetime)}
	f.refresh[ref] = &token{realm: realm, expires: now.Add(RefreshLifetime)}
	return qbo.Tokens{AccessToken: acc, RefreshToken: ref, AccessExpiresAt: now.Add(AccessLifetime), RefreshExpiresAt: now.Add(RefreshLifetime), RealmID: realm}
}

func (a *auth) Exchange(_ context.Context, code, redirectURI string) (qbo.Tokens, error) {
	f := a.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if a.id == "" || a.secret == "" {
		return qbo.Tokens{}, &qbo.Fault{HTTPStatus: http.StatusUnauthorized, Code: "invalid_client", Message: "client id and secret are required"}
	}
	realm, ok := f.codes[code]
	if !ok {
		return qbo.Tokens{}, &qbo.Fault{HTTPStatus: http.StatusBadRequest, Code: qbo.CodeInvalidGrant, Message: "unknown or used code"}
	}
	delete(f.codes, code)
	f.Exchanged = append(f.Exchanged, a.id+" "+redirectURI)
	return f.issue(realm), nil
}

func (a *auth) Refresh(_ context.Context, refreshToken string) (qbo.Tokens, error) {
	f := a.f
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.refresh[refreshToken]
	if f.revokeRef || t == nil || t.revoked {
		f.revokeRef = false
		return qbo.Tokens{}, &qbo.Fault{HTTPStatus: http.StatusBadRequest, Code: qbo.CodeInvalidGrant, Message: "the refresh token is invalid"}
	}
	t.revoked = true // rotated on every refresh
	return f.issue(t.realm), nil
}

func (a *auth) Revoke(_ context.Context, tok string) error {
	f := a.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.refresh[tok]; t != nil {
		t.revoked = true
		for _, at := range f.access {
			if at.realm == t.realm {
				at.revoked = true
			}
		}
	}
	return nil
}

// ---- Books ----

type books struct {
	f    *Fake
	conn qbo.Conn
	ts   qbo.TokenSource
}

// Open binds a fake company.
func (f *Fake) Open(conn qbo.Conn, ts qbo.TokenSource) qbo.Books {
	f.mu.Lock()
	if c := f.companies[conn.RealmID]; c != nil {
		c.envSeen = conn.Environment
	}
	f.mu.Unlock()
	return &books{f: f, conn: conn, ts: ts}
}

// errNotFound is a 610.
func errNotFound(k qbo.Kind, id string) error {
	return &qbo.Fault{HTTPStatus: http.StatusBadRequest, Code: qbo.CodeNotFound, Message: "Object Not Found: " + string(k) + " " + id}
}

// begin authorizes one call the way the HTTP adapter does: a 401 refreshes once.
func (b *books) begin(ctx context.Context, op string, k qbo.Kind, id, requestID string) (*company, error) {
	tok, err := b.ts.Token(ctx)
	if err != nil {
		return nil, err
	}
	if !b.f.AccessValid(tok) {
		if tok, err = b.ts.Refresh(ctx); err != nil {
			return nil, err
		}
		if !b.f.AccessValid(tok) {
			return nil, &qbo.Fault{HTTPStatus: http.StatusUnauthorized, Code: qbo.CodeUnauthorized, Message: "AuthenticationFailed"}
		}
	}
	b.f.mu.Lock()
	t := b.f.access[tok]
	c := b.f.companies[b.conn.RealmID]
	call := Call{Realm: b.conn.RealmID, Op: op, Kind: k, ID: id, RequestID: requestID}
	if t.realm != b.conn.RealmID || c == nil {
		b.f.calls = append(b.f.calls, call)
		b.f.mu.Unlock()
		return nil, &qbo.Fault{HTTPStatus: http.StatusForbidden, Code: "403", Message: "token is for another company"}
	}
	if b.f.throttle > zero {
		b.f.throttle--
		b.f.calls = append(b.f.calls, call)
		b.f.mu.Unlock()
		return nil, &qbo.Fault{HTTPStatus: http.StatusTooManyRequests, Code: "429", Message: "ThrottleExceeded", Retryable: true}
	}
	if requestID != "" {
		if _, ok := c.replays[requestID]; ok {
			call.Replayed = true
		}
	}
	b.f.calls = append(b.f.calls, call)
	return c, nil // f.mu stays locked; the caller unlocks
}

// write runs one write under the request id replay rule and the failure hooks.
func (b *books) write(ctx context.Context, op string, e qbo.Entity, k qbo.Kind, id, requestID string, apply func(c *company) (qbo.Ref, error)) (qbo.Ref, error) {
	c, err := b.begin(ctx, op, k, id, requestID)
	if err != nil {
		return qbo.Ref{}, err
	}
	defer b.f.mu.Unlock()
	if r, ok := c.replays[requestID]; ok && requestID != "" {
		return r.ref, r.err
	}
	if q := b.f.failNext[k]; len(q) > 0 {
		b.f.failNext[k] = q[one:]
		return qbo.Ref{}, q[0]
	}
	ref, err := apply(c)
	var fault *qbo.Fault
	if err == nil || (errors.As(err, &fault) && !fault.Retryable) {
		if requestID != "" {
			c.replays[requestID] = replay{ref: ref, err: err}
		}
	}
	if err == nil && b.f.timeout[k] > zero {
		b.f.timeout[k]--
		return qbo.Ref{}, &qbo.Fault{Code: qbo.CodeNetwork, Message: "timeout waiting for QuickBooks", Retryable: true}
	}
	_ = e
	return ref, err
}

func (b *books) Company(ctx context.Context) (qbo.Company, error) {
	c, err := b.begin(ctx, "company", "", "", "")
	if err != nil {
		return qbo.Company{}, err
	}
	defer b.f.mu.Unlock()
	return qbo.Company{RealmID: b.conn.RealmID, Name: c.name}, nil
}

func (b *books) Accounts(ctx context.Context) ([]qbo.Account, error) {
	c, err := b.begin(ctx, "query", "Account", "", "")
	if err != nil {
		return nil, err
	}
	defer b.f.mu.Unlock()
	return append([]qbo.Account(nil), c.accounts...), nil
}

func (b *books) Items(ctx context.Context) ([]qbo.Item, error) {
	c, err := b.begin(ctx, "query", "Item", "", "")
	if err != nil {
		return nil, err
	}
	defer b.f.mu.Unlock()
	return append([]qbo.Item(nil), c.items...), nil
}

func (b *books) PaymentMethods(ctx context.Context) ([]qbo.PaymentMethod, error) {
	c, err := b.begin(ctx, "query", "PaymentMethod", "", "")
	if err != nil {
		return nil, err
	}
	defer b.f.mu.Unlock()
	return append([]qbo.PaymentMethod(nil), c.methods...), nil
}

func (b *books) CustomerByName(ctx context.Context, name string) (*qbo.Customer, error) {
	c, err := b.begin(ctx, "query", qbo.KindCustomer, "", "")
	if err != nil {
		return nil, err
	}
	defer b.f.mu.Unlock()
	for _, s := range c.entities[qbo.KindCustomer] {
		if s.deleted {
			continue
		}
		cu := s.entity().(*qbo.Customer)
		if cu.DisplayName == name {
			return cu, nil
		}
	}
	return nil, nil
}

func (c *company) account(id string) *qbo.Account {
	for i := range c.accounts {
		if c.accounts[i].ID == id {
			return &c.accounts[i]
		}
	}
	return nil
}

func (c *company) item(id string) *qbo.Item {
	for i := range c.items {
		if c.items[i].ID == id {
			return &c.items[i]
		}
	}
	return nil
}

func businessFault(msg string) error {
	return &qbo.Fault{HTTPStatus: http.StatusBadRequest, Code: qbo.CodeBusinessRule, Message: "A business validation error has occurred: " + msg}
}

// validate applies the per-kind rules before a create or update lands.
func (c *company) validate(e qbo.Entity, selfID string) error {
	switch v := e.(type) {
	case *qbo.Customer:
		if v.DisplayName == "" || strings.Contains(v.DisplayName, ":") {
			return &qbo.Fault{HTTPStatus: http.StatusBadRequest, Code: "6000", Message: "DisplayName is required and can't hold a colon"}
		}
		if _, taken := c.others[v.DisplayName]; taken {
			return &qbo.Fault{HTTPStatus: http.StatusBadRequest, Code: qbo.CodeDuplicateName, Message: "Duplicate Name Exists Error: the name supplied already exists"}
		}
		for id, s := range c.entities[qbo.KindCustomer] {
			if id != selfID && !s.deleted && s.entity().(*qbo.Customer).DisplayName == v.DisplayName {
				return &qbo.Fault{HTTPStatus: http.StatusBadRequest, Code: qbo.CodeDuplicateName, Message: "Duplicate Name Exists Error: the name supplied already exists"}
			}
		}
	case *qbo.Invoice:
		if c.get(qbo.KindCustomer, v.CustomerRef.Value) == nil {
			return businessFault("invalid customer reference")
		}
		for _, l := range v.Line {
			if l.SalesItemLineDetail == nil || c.item(l.SalesItemLineDetail.ItemRef.Value) == nil {
				return businessFault("invalid item reference")
			}
		}
	case *qbo.CreditMemo:
		if c.get(qbo.KindCustomer, v.CustomerRef.Value) == nil {
			return businessFault("invalid customer reference")
		}
		for _, l := range v.Line {
			if l.SalesItemLineDetail == nil || c.item(l.SalesItemLineDetail.ItemRef.Value) == nil {
				return businessFault("invalid item reference")
			}
		}
	case *qbo.Payment:
		if c.get(qbo.KindCustomer, v.CustomerRef.Value) == nil {
			return businessFault("invalid customer reference")
		}
		if v.DepositToAccountRef != nil && c.account(v.DepositToAccountRef.Value) == nil {
			return businessFault("invalid deposit account")
		}
		for _, l := range v.Line {
			for _, lt := range l.LinkedTxn {
				if c.get(qbo.Kind(lt.TxnType), lt.TxnID) == nil {
					return businessFault("linked transaction " + lt.TxnType + " " + lt.TxnID + " doesn't exist")
				}
			}
		}
	case *qbo.JournalEntry:
		var dr, cr qbo.Money
		for _, l := range v.Line {
			d := l.JournalEntryLineDetail
			if d == nil || c.account(d.AccountRef.Value) == nil {
				return businessFault("journal line on an unknown account")
			}
			if d.Entity != nil && c.get(qbo.KindCustomer, d.Entity.EntityRef.Value) == nil {
				return businessFault("journal line on an unknown customer")
			}
			switch d.PostingType {
			case "Debit":
				dr += l.Amount
			case "Credit":
				cr += l.Amount
			default:
				return businessFault("posting type is Debit or Credit")
			}
		}
		if dr != cr || len(v.Line) < minJournalLines {
			return businessFault("Transaction must be balanced")
		}
	case *qbo.Purchase:
		if c.account(v.AccountRef.Value) == nil {
			return businessFault("invalid payment account")
		}
		for _, l := range v.Line {
			if l.AccountBasedExpenseLineDetail == nil || c.account(l.AccountBasedExpenseLineDetail.AccountRef.Value) == nil {
				return businessFault("invalid expense account")
			}
		}
	}
	return nil
}

func (b *books) Create(ctx context.Context, e qbo.Entity, requestID string) (qbo.Ref, error) {
	return b.write(ctx, "create", e, e.Kind(), "", requestID, func(c *company) (qbo.Ref, error) {
		if err := c.validate(e, ""); err != nil {
			return qbo.Ref{}, err
		}
		s := &stored{kind: e.Kind(), id: c.newID()}
		cp, _ := qbo.New(e.Kind())
		_ = json.Unmarshal(mustJSON(e), cp)
		cp.SetRef(qbo.Ref{})
		s.body = mustJSON(cp)
		c.put(s, b.f.now())
		return s.ref(), nil
	})
}

func (b *books) Update(ctx context.Context, e qbo.Entity, requestID string) (qbo.Ref, error) {
	r := e.GetRef()
	return b.write(ctx, "update", e, e.Kind(), r.ID, requestID, func(c *company) (qbo.Ref, error) {
		s := c.get(e.Kind(), r.ID)
		if s == nil {
			return qbo.Ref{}, errNotFound(e.Kind(), r.ID)
		}
		if r.SyncToken != strconv.Itoa(s.syncToken) {
			return qbo.Ref{}, &qbo.Fault{HTTPStatus: http.StatusBadRequest, Code: qbo.CodeStaleToken, Message: "Stale Object Error: You and someone else are working on the same object"}
		}
		if s.voided {
			return qbo.Ref{}, businessFault("a voided transaction can't change")
		}
		next, _ := qbo.New(e.Kind())
		if cu, ok := e.(*qbo.Customer); ok && cu.Sparse {
			// Sparse update: start from the stored entity and lay the sent fields over it.
			_ = json.Unmarshal(s.body, next)
		}
		_ = json.Unmarshal(mustJSON(e), next)
		if cu, ok := next.(*qbo.Customer); ok {
			cu.Sparse = false
		}
		if err := c.validate(next, r.ID); err != nil {
			return qbo.Ref{}, err
		}
		next.SetRef(qbo.Ref{})
		s.body = mustJSON(next)
		s.syncToken++
		c.put(s, b.f.now())
		return s.ref(), nil
	})
}

func (b *books) Void(ctx context.Context, k qbo.Kind, ref qbo.Ref, requestID string) (qbo.Ref, error) {
	return b.write(ctx, "void", nil, k, ref.ID, requestID, func(c *company) (qbo.Ref, error) {
		s := c.get(k, ref.ID)
		if s == nil {
			return qbo.Ref{}, errNotFound(k, ref.ID)
		}
		if ref.SyncToken != strconv.Itoa(s.syncToken) {
			return qbo.Ref{}, &qbo.Fault{HTTPStatus: http.StatusBadRequest, Code: qbo.CodeStaleToken, Message: "Stale Object Error"}
		}
		if k == qbo.KindInvoice {
			// QBO refuses to void an invoice that live payments are applied to.
			for _, ps := range c.entities[qbo.KindPayment] {
				if ps.deleted || ps.voided {
					continue
				}
				for _, l := range ps.entity().(*qbo.Payment).Line {
					for _, lt := range l.LinkedTxn {
						if lt.TxnType == "Invoice" && lt.TxnID == ref.ID && l.Amount != qbo.Money(zero) {
							return qbo.Ref{}, businessFault("the invoice has payments applied")
						}
					}
				}
			}
		}
		// Void keeps the record and zeroes its amounts.
		var m map[string]any
		_ = json.Unmarshal(s.body, &m)
		if lines, ok := m["Line"].([]any); ok {
			for _, l := range lines {
				l.(map[string]any)["Amount"] = zero
			}
		}
		if _, ok := m["TotalAmt"]; ok {
			m["TotalAmt"] = zero
		}
		m["PrivateNote"] = fmt.Sprint(m["PrivateNote"]) + " Voided"
		s.body = mustJSON(m)
		s.voided = true
		s.syncToken++
		c.put(s, b.f.now())
		return s.ref(), nil
	})
}

func (b *books) Delete(ctx context.Context, k qbo.Kind, ref qbo.Ref, requestID string) error {
	_, err := b.write(ctx, "delete", nil, k, ref.ID, requestID, func(c *company) (qbo.Ref, error) {
		s := c.get(k, ref.ID)
		if s == nil {
			return qbo.Ref{}, errNotFound(k, ref.ID)
		}
		if ref.SyncToken != strconv.Itoa(s.syncToken) {
			return qbo.Ref{}, &qbo.Fault{HTTPStatus: http.StatusBadRequest, Code: qbo.CodeStaleToken, Message: "Stale Object Error"}
		}
		s.deleted = true
		s.syncToken++
		c.put(s, b.f.now())
		return qbo.Ref{ID: s.id}, nil
	})
	return err
}

// decorate adds the read-only amounts QBO computes: totals and balances.
func (f *Fake) decorate(c *company, s *stored) qbo.Entity {
	e := s.entity()
	switch v := e.(type) {
	case *qbo.Invoice:
		total := sumLines(v.Line)
		bal := total - c.applied("Invoice", s.id)
		v.TotalAmt, v.Balance = &total, &bal
	case *qbo.CreditMemo:
		total := sumLines(v.Line)
		rem := total - c.applied("CreditMemo", s.id)
		v.TotalAmt, v.RemainingCredit = &total, &rem
	case *qbo.Purchase:
		total := sumLines(v.Line)
		v.TotalAmt = &total
	}
	return e
}

func sumLines(ls []qbo.Line) qbo.Money {
	var t qbo.Money
	for _, l := range ls {
		t += l.Amount
	}
	return t
}

// applied is how much live payments apply to a transaction.
func (c *company) applied(txnType, id string) qbo.Money {
	var t qbo.Money
	for _, ps := range c.entities[qbo.KindPayment] {
		if ps.deleted || ps.voided {
			continue
		}
		for _, l := range ps.entity().(*qbo.Payment).Line {
			for _, lt := range l.LinkedTxn {
				if lt.TxnType == txnType && lt.TxnID == id {
					t += l.Amount
				}
			}
		}
	}
	return t
}

// InvoiceBalance is QBO's balance for an invoice.
func (f *Fake) InvoiceBalance(realm, id string) qbo.Money {
	e := f.Get(realm, qbo.KindInvoice, id)
	if e == nil {
		return qbo.Money(zero)
	}
	return *e.(*qbo.Invoice).Balance
}

func (b *books) Read(ctx context.Context, k qbo.Kind, id string) (qbo.Entity, qbo.Ref, error) {
	c, err := b.begin(ctx, "read", k, id, "")
	if err != nil {
		return nil, qbo.Ref{}, err
	}
	defer b.f.mu.Unlock()
	s := c.get(k, id)
	if s == nil {
		return nil, qbo.Ref{}, errNotFound(k, id)
	}
	return b.f.decorate(c, s), s.ref(), nil
}

func (b *books) Changes(ctx context.Context, kinds []qbo.Kind, since time.Time) ([]qbo.Change, error) {
	c, err := b.begin(ctx, "changes", "", "", "")
	if err != nil {
		return nil, err
	}
	defer b.f.mu.Unlock()
	want := map[qbo.Kind]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	latest := map[string]qbo.Change{}
	var order []string
	for _, ch := range c.changes {
		if !want[ch.Kind] || ch.UpdatedAt.Before(since) {
			continue
		}
		key := string(ch.Kind) + "/" + ch.ID
		if _, ok := latest[key]; !ok {
			order = append(order, key)
		}
		latest[key] = ch
	}
	out := make([]qbo.Change, 0, len(order))
	for _, k := range order {
		out = append(out, latest[k])
	}
	return out, nil
}

func (b *books) CreateAccount(ctx context.Context, a qbo.Account, requestID string) (qbo.Ref, error) {
	return b.write(ctx, "create_account", nil, "Account", "", requestID, func(c *company) (qbo.Ref, error) {
		for _, x := range c.accounts {
			if x.Name == a.Name {
				return qbo.Ref{}, &qbo.Fault{HTTPStatus: http.StatusBadRequest, Code: qbo.CodeDuplicateName, Message: "Duplicate Name Exists Error"}
			}
		}
		a.ID, a.FullyQualifiedName, a.Active = c.newID(), a.Name, true
		c.accounts = append(c.accounts, a)
		return qbo.Ref{ID: a.ID, SyncToken: "0"}, nil
	})
}

func (b *books) CreateItem(ctx context.Context, i qbo.Item, requestID string) (qbo.Ref, error) {
	return b.write(ctx, "create_item", nil, "Item", "", requestID, func(c *company) (qbo.Ref, error) {
		for _, x := range c.items {
			if x.Name == i.Name {
				return qbo.Ref{}, &qbo.Fault{HTTPStatus: http.StatusBadRequest, Code: qbo.CodeDuplicateName, Message: "Duplicate Name Exists Error"}
			}
		}
		if i.IncomeAccountRef == nil || c.account(i.IncomeAccountRef.Value) == nil {
			return qbo.Ref{}, businessFault("an item needs an income account")
		}
		i.ID, i.FullyQualifiedName, i.Active = c.newID(), i.Name, true
		c.items = append(c.items, i)
		return qbo.Ref{ID: i.ID, SyncToken: "0"}, nil
	})
}

// EnvironmentSeen is the environment the module last opened realm with.
func (f *Fake) EnvironmentSeen(realm string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.companies[realm].envSeen
}

// Balances returns each account's balance in cents, debit positive, from every live
// transaction.
func (f *Fake) Balances(realm string) map[string]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.companies[realm]
	out := map[string]int64{}
	ar := ""
	undeposited := ""
	for _, a := range c.accounts {
		if a.AccountType == "Accounts Receivable" {
			ar = a.ID
		}
		if a.AccountSubType == "UndepositedFunds" {
			undeposited = a.ID
		}
	}
	income := func(itemID string) string {
		if it := c.item(itemID); it != nil && it.IncomeAccountRef != nil {
			return it.IncomeAccountRef.Value
		}
		return ""
	}
	for _, k := range []qbo.Kind{qbo.KindInvoice, qbo.KindCreditMemo, qbo.KindPayment, qbo.KindJournalEntry, qbo.KindPurchase} {
		for _, s := range c.entities[k] {
			if s.deleted {
				continue
			}
			switch v := s.entity().(type) {
			case *qbo.Invoice:
				for _, l := range v.Line {
					out[ar] += int64(l.Amount)
					out[income(l.SalesItemLineDetail.ItemRef.Value)] -= int64(l.Amount)
				}
			case *qbo.CreditMemo:
				for _, l := range v.Line {
					out[ar] -= int64(l.Amount)
					out[income(l.SalesItemLineDetail.ItemRef.Value)] += int64(l.Amount)
				}
			case *qbo.Payment:
				dep := undeposited
				if v.DepositToAccountRef != nil {
					dep = v.DepositToAccountRef.Value
				}
				out[dep] += int64(v.TotalAmt)
				out[ar] -= int64(v.TotalAmt)
			case *qbo.JournalEntry:
				for _, l := range v.Line {
					d := l.JournalEntryLineDetail
					if d.PostingType == "Debit" {
						out[d.AccountRef.Value] += int64(l.Amount)
					} else {
						out[d.AccountRef.Value] -= int64(l.Amount)
					}
				}
			case *qbo.Purchase:
				for _, l := range v.Line {
					out[l.AccountBasedExpenseLineDetail.AccountRef.Value] += int64(l.Amount)
					out[v.AccountRef.Value] -= int64(l.Amount)
				}
			}
		}
	}
	delete(out, "")
	return out
}

// AccountType returns an account's type, for assertions.
func (f *Fake) AccountType(realm, id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.companies[realm].account(id); a != nil {
		return a.AccountType
	}
	return ""
}

// AccountsList returns the company's accounts.
func (f *Fake) AccountsList(realm string) []qbo.Account {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]qbo.Account(nil), f.companies[realm].accounts...)
}

// ItemsList returns the company's items.
func (f *Fake) ItemsList(realm string) []qbo.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]qbo.Item(nil), f.companies[realm].items...)
}
