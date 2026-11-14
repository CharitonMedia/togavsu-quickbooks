package quickbooks_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	quickbooks "github.com/CharitonMedia/togavsu-quickbooks"
	"github.com/CharitonMedia/togavsu-quickbooks/internal/fakecore"
	"github.com/CharitonMedia/togavsu-quickbooks/internal/wire"
	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
	"github.com/CharitonMedia/togavsu-quickbooks/qbo/fake"
)

// The acceptance suite runs against the fake core, the fake QBO and a real Postgres
// database named after the repo (togavsu_quickbooks). Records are synthetic: firm Sample
// Law Co, client Jane Sample, matter 2026-0042.

const (
	realmA = "9130350000000001"
	realmB = "9130350000000002"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	t      *testing.T
	ctx    context.Context
	dsn    string
	db     *sql.DB // admin connection (the core's side)
	modDB  *sql.DB // the module's own connection, search_path mod_quickbooks
	clock  *clock
	core   *fakecore.Core
	qb     *fake.Fake
	srv    *quickbooks.Server
	h      http.Handler
	logBuf *bytes.Buffer

	admin, accountant, attorney, staff string
	jane, matter, trust                string
	clientSecret                       string
}

func testDSN(t *testing.T) string {
	dsn := os.Getenv("TOGAVSU_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TOGAVSU_TEST_DATABASE_URL must be set in CI")
		}
		t.Skip("TOGAVSU_TEST_DATABASE_URL is not set")
	}
	return dsn
}

func withParam(dsn, k, v string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		panic(err)
	}
	q := u.Query()
	q.Set(k, v)
	u.RawQuery = q.Encode()
	return u.String()
}

// requestedScopes reads the permission list from the manifest text.
func requestedScopes(t *testing.T) []string {
	files, err := quickbooks.Files()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(files["togavsu-module.yaml"]), "\n") {
		if s, ok := strings.CutPrefix(strings.TrimSpace(line), "- scope: "); ok {
			out = append(out, s)
		}
	}
	return out
}

func migrations(t *testing.T) map[string][]byte {
	files, err := quickbooks.Files()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for name, b := range files {
		if n, ok := strings.CutPrefix(name, "migrations/"); ok {
			out[n] = b
		}
	}
	return out
}

// newEnv is a connected environment; newUnconnected leaves QuickBooks unconnected.
func newEnv(t *testing.T) *env { t.Helper(); return setup(t, true) }

func newUnconnected(t *testing.T) *env { t.Helper(); return setup(t, false) }

func setup(t *testing.T, connect bool) *env {
	t.Helper()
	dsn := testDSN(t)
	e := &env{t: t, ctx: context.Background(), dsn: dsn, clock: &clock{t: time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)}, logBuf: &bytes.Buffer{}}
	var err error
	if e.db, err = sql.Open("pgx", dsn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.db.Close() })
	if err := fakecore.Reset(e.ctx, e.db); err != nil {
		t.Fatal(err)
	}
	e.core = fakecore.New(e.db, e.clock.Now)
	t.Cleanup(e.core.Close)
	if err := e.core.Install(e.ctx, requestedScopes(t), requestedScopes(t), migrations(t)); err != nil {
		t.Fatal(err)
	}
	if e.modDB, err = sql.Open("pgx", withParam(dsn, "search_path", "mod_quickbooks")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.modDB.Close() })
	e.qb = fake.New()
	e.qb.SetClock(e.clock.Now)
	e.qb.Seed(realmA, fake.SampleChart())
	e.qb.Seed(realmB, fake.SampleChart())
	e.srv = quickbooks.New(quickbooks.Config{Core: e.core.URL(), Token: e.core.Token, WebhookSecret: e.core.WebhookSecret,
		DB: e.modDB, QBO: e.qb, Now: e.clock.Now, Log: e.logBuf})
	e.h = e.srv.Handler()
	e.core.Module = e.h

	e.admin = e.core.AddUser("Avery Admin", "admin")
	e.accountant = e.core.AddUser("Casey Accountant", "accountant")
	e.attorney = e.core.AddUser("Riley Attorney", "attorney")
	e.staff = e.core.AddUser("Sam Staff", "staff")
	e.jane = e.core.AddContact(quickbooks.BooksContact{GivenName: "Jane", FamilyName: "Sample", Email: "jane@example.com",
		Phone: "555-0100", BillingAddress: &quickbooks.BooksAddress{Line1: "1 Sample Way", City: "Springfield", Region: "IL", PostalCode: "62701", Country: "US"}})
	e.matter = e.core.AddMatter("2026-0042", "Sample v. Example", e.jane)
	e.trust = e.core.AddTrustAccount("IOLTA Trust")
	e.core.Settings["qbo_client_id"] = "client-" + time.Now().Format("150405.000")
	e.core.Settings["account_operating_bank"] = "Operating Checking"
	e.core.Settings["account_trust_bank"] = "IOLTA Trust"
	e.core.Settings["sync_start_date"] = "2026-01-01"
	e.clientSecret = "secret-" + strings.ReplaceAll(e.core.ContextToken(e.admin, ""), "ctx_", "")
	if connect {
		if r := e.connect(realmA); r.status != http.StatusOK {
			t.Fatalf("connect: %d %s", r.status, r.body)
		}
	}
	return e
}

// ctxFor is a context token for a user, on a matter or none.
func (e *env) ctxFor(user, matter string) string { return e.core.ContextToken(user, matter) }

type resp struct {
	status int
	body   string
	header http.Header
}

func (e *env) do(method, path, tok string, body io.Reader, ctype string) resp {
	req := httptest.NewRequest(method, path, body)
	if tok != "" {
		req.Header.Set(wire.ContextHeader, tok)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Host = "firm.example.test"
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return resp{status: rec.Code, body: rec.Body.String(), header: rec.Header()}
}

func (e *env) get(path, tok string) resp { return e.do(http.MethodGet, path, tok, nil, "") }

// act posts an agent action as JSON.
func (e *env) act(path, tok string, body map[string]string) resp {
	raw, _ := json.Marshal(body)
	return e.do(http.MethodPost, "/api/"+path, tok, bytes.NewReader(raw), "application/json")
}

func (e *env) mustAct(path, tok string, body map[string]string) {
	e.t.Helper()
	if r := e.act(path, tok, body); r.status != http.StatusOK {
		e.t.Fatalf("%s: %d %s", path, r.status, r.body)
	}
}

func decode[T any](t *testing.T, r resp) T {
	t.Helper()
	var v T
	if r.status != http.StatusOK {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	if err := json.Unmarshal([]byte(r.body), &v); err != nil {
		t.Fatalf("%v: %s", err, r.body)
	}
	return v
}

// connect runs the whole OAuth flow as the admin.
func (e *env) connect(realm string) resp {
	e.t.Helper()
	admin := e.ctxFor(e.admin, "")
	if r := e.act("client-secret", admin, map[string]string{"client_secret": e.clientSecret}); r.status != http.StatusOK {
		e.t.Fatalf("client secret: %d %s", r.status, r.body)
	}
	setup := decode[quickbooks.SetupView](e.t, e.get("/api/setup", admin))
	if setup.ConnectURL == "" {
		e.t.Fatal("no connect URL for the admin")
	}
	start := e.get("/"+setup.ConnectURL, "")
	if start.status != http.StatusFound {
		e.t.Fatalf("start: %d %s", start.status, start.body)
	}
	loc, err := url.Parse(start.header.Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}
	q := url.Values{"state": {loc.Query().Get("state")}, "code": {e.qb.IssueCode(realm)}, "realmId": {realm}}
	return e.get("/oauth/callback?"+q.Encode(), "")
}

func (e *env) tick() {
	e.t.Helper()
	if err := e.srv.Tick(e.ctx); err != nil {
		e.t.Fatalf("tick: %v", err)
	}
}

// syncNow presses Sync now as the admin.
func (e *env) syncNow() {
	e.t.Helper()
	e.mustAct("sync-now", e.ctxFor(e.admin, ""), nil)
}

type rowState struct {
	State, SkipReason, WaitingOn, QBOID, Code, Error, RequestID string
	Attempts                                                    int
	NextAttemptAt                                               int64
}

func (e *env) row(table, key, id string) (rowState, bool) {
	e.t.Helper()
	var r rowState
	err := e.modDB.QueryRow(`SELECT state, skip_reason, waiting_on, qbo_id, last_error_code, last_error, request_id, attempts, next_attempt_at FROM `+table+` WHERE `+key+` = $1`, id).
		Scan(&r.State, &r.SkipReason, &r.WaitingOn, &r.QBOID, &r.Code, &r.Error, &r.RequestID, &r.Attempts, &r.NextAttemptAt)
	if err == sql.ErrNoRows {
		return r, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return r, true
}

func (e *env) invoiceRow(id string) rowState { return e.must(e.row("invoice_sync", "invoice_id", id)) }
func (e *env) paymentRow(id string) rowState { return e.must(e.row("payment_sync", "payment_id", id)) }
func (e *env) trustRow(id string) rowState {
	return e.must(e.row("trust_entry_sync", "trust_entry_id", id))
}
func (e *env) expenseRow(id string) rowState { return e.must(e.row("expense_sync", "expense_id", id)) }

func (e *env) must(r rowState, ok bool) rowState {
	e.t.Helper()
	if !ok {
		e.t.Fatal("no sync row")
	}
	return r
}

func (e *env) customerID(contact string) string {
	e.t.Helper()
	var id string
	if err := e.modDB.QueryRow(`SELECT qbo_id FROM customer_sync WHERE contact_id = $1`, contact).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.modDB.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) entities(k qbo.Kind) []qbo.Entity { return e.qb.Entities(realmA, k) }

func (e *env) writes() int { return len(e.qb.Writes()) }

// sentInvoice creates and sends an invoice of one time line.
func (e *env) sentInvoice(matter, number string, cents int64) string {
	id := e.core.CreateInvoice(fakecore.InvoiceInput{MatterID: matter, Number: number, IssuedOn: "2026-03-01",
		Lines: []quickbooks.InvoiceLine{{Kind: "time", Description: "Drafting", AmountCents: cents}}})
	e.core.SendInvoice(id)
	return id
}

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
		s := d.PostingType + " " + d.AccountRef.Value + " " + itoa(int64(l.Amount))
		if d.Entity != nil {
			s += " customer " + d.Entity.EntityRef.Value
		}
		out = append(out, s)
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
