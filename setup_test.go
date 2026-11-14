package quickbooks_test

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	quickbooks "github.com/CharitonMedia/togavsu-quickbooks"
	"github.com/CharitonMedia/togavsu-quickbooks/internal/fakecore"
	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
)

func manifest(t *testing.T) string {
	t.Helper()
	files, err := quickbooks.Files()
	if err != nil {
		t.Fatal(err)
	}
	return string(files["togavsu-module.yaml"])
}

func TestMODQBO01Install(t *testing.T) {
	e := newUnconnected(t)
	m := manifest(t)
	scopes := requestedScopes(t)
	if len(scopes) != 10 || strings.Count(m, "    reason: ") != 10 {
		t.Fatalf("scopes %v", scopes)
	}
	for _, field := range []string{"author: Chariton Media", "price: Free", "license: Same as Togavsu", `version: "1.0.0"`} {
		if !strings.Contains(m, field) {
			t.Fatalf("manifest lacks %q", field)
		}
	}
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM core.module_migrations WHERE module = 'quickbooks'`).Scan(&n); err != nil || n != len(migrations(t)) {
		t.Fatalf("migrations recorded %d (%v)", n, err)
	}
	// declining books:read cancels the install and creates nothing
	if err := fakecore.Reset(e.ctx, e.db); err != nil {
		t.Fatal(err)
	}
	var approved []string
	for _, s := range scopes {
		if s != "books:read" {
			approved = append(approved, s)
		}
	}
	if err := e.core.Install(e.ctx, scopes, approved, migrations(t)); !errors.Is(err, fakecore.ErrDeclined) {
		t.Fatalf("install with books:read declined: %v", err)
	}
	var schema sql.NullString
	_ = e.db.QueryRow(`SELECT to_regnamespace('mod_quickbooks')::text`).Scan(&schema)
	if schema.Valid {
		t.Fatal("the schema exists after a cancelled install")
	}
}

func TestMODQBO02ForeignKeys(t *testing.T) {
	e := newEnv(t)
	rows, err := e.db.Query(`SELECT k.conname, k.confdeltype, f.relnamespace::regnamespace::text FROM pg_constraint k
JOIN pg_class f ON f.oid = k.confrelid WHERE k.contype = 'f' AND k.connamespace = 'mod_quickbooks'::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	core := 0
	for rows.Next() {
		var name, del, ns string
		if err := rows.Scan(&name, &del, &ns); err != nil {
			t.Fatal(err)
		}
		if ns == "core" {
			core++
			if del != "r" {
				t.Fatalf("%s is not ON DELETE RESTRICT (%s)", name, del)
			}
		}
	}
	rows.Close()
	if core < 17 {
		t.Fatalf("only %d core foreign keys", core)
	}
	e.core.PostTrust(e.matter, e.trust, "deposit", 1000, "Retainer")
	e.tick()
	err = e.core.HardDeleteContact(e.jane)
	if err == nil || !strings.Contains(err.Error(), "module quickbooks") {
		t.Fatalf("hard delete: %v", err)
	}
}

func TestMODQBO03Widget(t *testing.T) {
	e := newEnv(t)
	m := manifest(t)
	if !strings.Contains(m, "    target:\n      tab: books\n      params: [status]") {
		t.Fatal("widget target is not the books tab")
	}
	e.sentInvoice(e.matter, "INV-8001", 1000)
	e.tick()
	w := decode[quickbooks.WidgetView](t, e.get("/api/status", e.ctxFor(e.accountant, "")))
	if w.Counts.Synced != 1 {
		t.Fatalf("counts %+v", w.Counts)
	}
	for _, s := range []string{"synced", "pending", "exceptions"} {
		if w.Links[s] != "/modules/quickbooks/tabs/books?status="+s {
			t.Fatalf("link %s = %q", s, w.Links[s])
		}
	}
	page := e.get("/widget", e.ctxFor(e.admin, ""))
	if page.status != http.StatusOK || !strings.Contains(page.body, `href="/modules/quickbooks/tabs/books?status=exceptions"`) {
		t.Fatalf("widget page %d %s", page.status, page.body)
	}
	if r := e.get("/widget", e.ctxFor(e.staff, "")); r.status != http.StatusNotFound {
		t.Fatalf("widget for staff %d", r.status)
	}
}

// dumpModule renders every row of every mod_quickbooks table as text.
func dumpModule(t *testing.T, e *env) string {
	t.Helper()
	var tables []string
	rows, err := e.db.Query(`SELECT table_name FROM information_schema.tables WHERE table_schema = 'mod_quickbooks'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	var b strings.Builder
	for _, tb := range tables {
		rs, err := e.db.Query(`SELECT t::text FROM mod_quickbooks.` + tb + ` t`)
		if err != nil {
			t.Fatal(err)
		}
		for rs.Next() {
			var s string
			_ = rs.Scan(&s)
			b.WriteString(s + "\n")
		}
		rs.Close()
	}
	return b.String()
}

func TestMODQBO04OAuth(t *testing.T) {
	e := newUnconnected(t)
	admin := e.ctxFor(e.admin, "")
	e.mustAct("client-secret", admin, map[string]string{"client_secret": e.clientSecret})
	setupPage := e.get("/setup", admin)
	if !strings.Contains(setupPage.body, `target="_blank" rel="noopener" data-connect`) || !strings.Contains(setupPage.body, "https://firm.example.test/m/quickbooks/oauth/callback") {
		t.Fatalf("setup page %s", setupPage.body)
	}
	setup := decode[quickbooks.SetupView](t, e.get("/api/setup", admin))
	start := e.get("/"+setup.ConnectURL, "")
	loc, _ := url.Parse(start.header.Get("Location"))
	if start.status != http.StatusFound || loc.Host != "appcenter.intuit.com" {
		t.Fatalf("start %d %s", start.status, loc)
	}
	state := loc.Query().Get("state")
	cb := e.get("/oauth/callback?"+url.Values{"state": {state}, "code": {e.qb.IssueCode(realmA)}, "realmId": {realmA}}.Encode(), "")
	if cb.status != http.StatusOK {
		t.Fatalf("callback %d %s", cb.status, cb.body)
	}
	access, refresh := e.core.Secrets["qbo/access_token"], e.core.Secrets["qbo/refresh_token"]
	if access == "" || refresh == "" || !e.qb.AccessValid(access) {
		t.Fatal("tokens not stored through the secret store")
	}
	// a reused state is refused
	if r := e.get("/oauth/callback?"+url.Values{"state": {state}, "code": {e.qb.IssueCode(realmA)}}.Encode(), ""); r.status != http.StatusForbidden {
		t.Fatalf("reused state %d", r.status)
	}
	// an expired one too
	setup = decode[quickbooks.SetupView](t, e.get("/api/setup", admin))
	e.clock.Advance(11 * time.Minute)
	if r := e.get("/"+setup.ConnectURL, ""); r.status != http.StatusForbidden {
		t.Fatalf("expired state %d", r.status)
	}
	// a non-admin gets no connect link and cannot store a secret
	acct := e.ctxFor(e.accountant, "")
	if v := decode[quickbooks.SetupView](t, e.get("/api/setup", acct)); v.ConnectURL != "" {
		t.Fatal("accountant got a connect link")
	}
	if r := e.act("client-secret", acct, map[string]string{"client_secret": "x"}); r.status != http.StatusForbidden {
		t.Fatalf("accountant client secret %d", r.status)
	}
	e.sentInvoice(e.matter, "INV-9001", 1000)
	e.tick()
	var responses strings.Builder
	for _, p := range []string{"/api/setup", "/setup", "/api/books", "/books", "/api/status", "/widget"} {
		responses.WriteString(e.get(p, admin).body)
	}
	responses.WriteString(e.get("/api/openapi.json", "").body)
	for name, hay := range map[string]string{"module tables": dumpModule(t, e), "log": e.logBuf.String(), "responses": responses.String() + cb.body} {
		for _, secret := range []string{e.core.Secrets["qbo/access_token"], e.core.Secrets["qbo/refresh_token"], access, refresh, e.clientSecret} {
			if strings.Contains(hay, secret) {
				t.Fatalf("the %s hold a token or the client secret", name)
			}
		}
	}
	for _, w := range e.core.SecretWrites {
		if w != "qbo/client_secret" && w != "qbo/access_token" && w != "qbo/refresh_token" {
			t.Fatalf("unexpected secret %s", w)
		}
	}
}

func TestMODQBO05Refresh(t *testing.T) {
	e := newEnv(t)
	oldRefresh := e.core.Secrets["qbo/refresh_token"]
	e.clock.Advance(2 * time.Hour)
	inv := e.sentInvoice(e.matter, "INV-9101", 1000)
	e.tick()
	if e.invoiceRow(inv).State != "synced" {
		t.Fatal("push after expiry failed")
	}
	if nr := e.core.Secrets["qbo/refresh_token"]; nr == oldRefresh || !e.qb.RefreshValid(nr) || e.qb.RefreshValid(oldRefresh) {
		t.Fatal("the rotated refresh token was not stored")
	}
	e.qb.RevokeRefresh()
	e.clock.Advance(2 * time.Hour)
	inv2 := e.sentInvoice(e.matter, "INV-9102", 1000)
	e.tick()
	setup := decode[quickbooks.SetupView](t, e.get("/api/setup", e.ctxFor(e.admin, "")))
	if setup.Connection.State != "reconnect_needed" {
		t.Fatalf("state %s", setup.Connection.State)
	}
	if r := e.invoiceRow(inv2); r.State != "pending" || r.Attempts != 0 {
		t.Fatalf("paused row %+v", r)
	}
	last := e.core.Notifications[len(e.core.Notifications)-1]
	if !equal(last.Roles, []string{"admin"}) || !equal(last.Recipients, []string{e.admin}) {
		t.Fatalf("alert %+v", last)
	}
	e.clock.Advance(time.Minute)
	e.tick()
	if r := e.invoiceRow(inv2); r.State != "pending" {
		t.Fatalf("pushed while paused %+v", r)
	}
}

func TestMODQBO29Purchases(t *testing.T) {
	e := newEnv(t)
	lines := e.core.AddExpense(e.matter, "2026-03-01", "Court reporter", 32000, true)
	e.tick()
	if r := e.expenseRow(lines); r.State != "skipped" || len(e.entities(qbo.KindPurchase)) != 0 {
		t.Fatalf("invoice_lines_only %+v", r)
	}
	e.core.Settings["expense_push"] = "purchases"
	e.core.Settings["account_expense_paid_from"] = "Firm Card"
	x := e.core.AddExpense(e.matter, "2026-03-02", "Courier", 4500, false)
	e.tick()
	e.tick()
	ps := e.entities(qbo.KindPurchase)
	if len(ps) != 1 {
		t.Fatalf("%d purchases", len(ps))
	}
	p := ps[0].(*qbo.Purchase)
	d := p.Line[0].AccountBasedExpenseLineDetail
	if p.AccountRef.Value != "41" || p.PaymentType != "CreditCard" || d.AccountRef.Value != "91" || d.BillableStatus != "NotBillable" || d.CustomerRef.Value != e.customerID(e.jane) {
		t.Fatalf("purchase %+v %+v", p, d)
	}
	if e.expenseRow(x).State != "synced" {
		t.Fatal("expense not synced")
	}
}

func TestMODQBO30Sandbox(t *testing.T) {
	e := newEnv(t)
	e.tick()
	if got := e.qb.EnvironmentSeen(realmA); got != "sandbox" {
		t.Fatalf("environment %q", got)
	}
	hp := &qbo.HTTPProvider{Endpoints: qbo.IntuitEndpoints()}
	if hp.BaseURL("sandbox") != "https://sandbox-quickbooks.api.intuit.com" || hp.BaseURL("production") != "https://quickbooks.api.intuit.com" {
		t.Fatal("base URLs")
	}
	admin := e.ctxFor(e.admin, e.matter)
	for _, p := range []string{"/widget", "/books", "/setup", "/matter"} {
		r := e.get(p, admin)
		if r.status != http.StatusOK || !strings.Contains(r.body, `data-banner="sandbox"`) {
			t.Fatalf("%s: %d banner missing", p, r.status)
		}
	}
	w := decode[quickbooks.WidgetView](t, e.get("/api/status", admin))
	if !w.Sandbox {
		t.Fatal("widget does not say Sandbox")
	}
	e.core.Settings["qbo_environment"] = "production"
	if r := e.get("/books", admin); strings.Contains(r.body, `data-banner="sandbox"`) {
		t.Fatal("banner in production")
	}
}

func TestMODQBO31RealmSwitch(t *testing.T) {
	e := newEnv(t)
	e.sentInvoice(e.matter, "INV-9301", 1000)
	e.tick()
	logs := e.count(`SELECT count(*) FROM sync_log`)
	admin := e.ctxFor(e.admin, "")
	e.mustAct("disconnect", admin, nil)
	if _, ok := e.core.Secrets["qbo/access_token"]; ok {
		t.Fatal("tokens kept after disconnect")
	}
	if r := e.connect(realmB); r.status != http.StatusConflict {
		t.Fatalf("realm switch %d %s", r.status, r.body)
	}
	if r := e.act("reset", admin, map[string]string{"confirm": "reset"}); r.status != http.StatusBadRequest {
		t.Fatalf("reset without RESET %d", r.status)
	}
	e.mustAct("reset", admin, map[string]string{"confirm": "RESET"})
	for _, tb := range []string{"customer_sync", "invoice_sync", "payment_sync", "trust_entry_sync", "expense_sync"} {
		if n := e.count(`SELECT count(*) FROM ` + tb); n != 0 {
			t.Fatalf("%s holds %d rows", tb, n)
		}
	}
	if n := e.count(`SELECT count(*) FROM sync_log`); n != logs {
		t.Fatalf("sync_log %d, was %d", n, logs)
	}
	if r := e.connect(realmB); r.status != http.StatusOK {
		t.Fatalf("connect after reset %d", r.status)
	}
}

func TestMODQBO32Include(t *testing.T) {
	e := newEnv(t)
	inv := e.sentInvoice(e.matter, "INV-9401", 1000)
	e.tick()
	var got quickbooks.Invoice
	if err := coreGet(t, e, "/api/v1/invoices/"+inv+"?include=quickbooks.invoice_sync", e.ctxFor(e.accountant, ""), &got); err != nil {
		t.Fatal(err)
	}
	rows := got.Included["quickbooks.invoice_sync"]
	if len(rows) != 1 || rows[0]["state"] != "synced" {
		t.Fatalf("included %+v", got.Included)
	}
	wally := e.core.AddContact(quickbooks.BooksContact{GivenName: "Wally", FamilyName: "Walled"})
	m := e.core.AddMatter("2026-0043", "Walled matter", wally)
	e.core.Wall(m, e.attorney)
	hidden := e.sentInvoice(m, "INV-9402", 1000)
	if err := coreGet(t, e, "/api/v1/invoices/"+hidden+"?include=quickbooks.invoice_sync", e.ctxFor(e.accountant, ""), &got); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("walled invoice for the accountant: %v", err)
	}
}

func TestMODQBO33TotalsCheck(t *testing.T) {
	e := newEnv(t)
	dep := e.core.PostTrust(e.matter, e.trust, "deposit", 150000, "Retainer")
	e.tick()
	e.qb.AlterJournalEntry(realmA, e.trustRow(dep).QBOID, 140000)
	e.clock.Advance(time.Minute)
	e.syncNow()
	r := e.trustRow(dep)
	if r.State != "drift" || r.Code != "totals_mismatch" || !strings.Contains(r.Error, e.trust) || !strings.Contains(r.Error, "-100.00") {
		t.Fatalf("row %+v", r)
	}
}

func TestMODQBO34TunableSettings(t *testing.T) {
	e := newEnv(t)
	m := manifest(t)
	for _, want := range []string{"key: retry_schedule", "default: 1m,5m,30m,2h,6h", "key: alert_interval_minutes", `default: "15"`} {
		if !strings.Contains(m, want) {
			t.Fatalf("manifest lacks %q", want)
		}
	}
	e.core.Settings["retry_schedule"] = "3m,7m"
	inv := e.sentInvoice(e.matter, "INV-9501", 1000)
	e.qb.FailNext(qbo.KindInvoice, unavailable())
	e.tick()
	if r := e.invoiceRow(inv); r.NextAttemptAt != e.clock.Now().Add(3*time.Minute).UnixMicro() {
		t.Fatalf("next attempt %+v", r)
	}
	e.qb.FailNext(qbo.KindInvoice, unavailable())
	e.clock.Advance(3 * time.Minute)
	e.tick()
	if r := e.invoiceRow(inv); r.State != "failed" {
		t.Fatalf("two-step schedule %+v", r)
	}
	alerts := func() int {
		n := 0
		for _, x := range e.core.Notifications {
			if strings.HasSuffix(x.Link, "status=exceptions") {
				n++
			}
		}
		return n
	}
	if alerts() != 1 {
		t.Fatalf("%d alerts", alerts())
	}
	inv2 := e.sentInvoice(e.matter, "INV-9502", 1000)
	e.qb.FailNext(qbo.KindInvoice, &qbo.Fault{HTTPStatus: 400, Code: "6000", Message: "business rule"})
	e.clock.Advance(time.Minute)
	e.tick()
	if e.invoiceRow(inv2).State != "failed" || alerts() != 1 {
		t.Fatalf("alert inside the interval: %d", alerts())
	}
	e.core.Settings["alert_interval_minutes"] = "1"
	e.clock.Advance(time.Minute)
	e.tick()
	if alerts() != 2 {
		t.Fatalf("after shortening the interval: %d alerts", alerts())
	}
}
