package acceptance_test

import (
	"database/sql"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	quickbooks "github.com/CharitonMedia/togavsu-quickbooks"
	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
	"github.com/CharitonMedia/togavsu/pkg/modulekit/coretest"
)

func TestMODQBO01Install(t *testing.T) {
	s := newSuite(t)
	if dep := s.c.Last(id); dep.Version != "1.0.0" || !s.c.SchemaExists(id) {
		t.Fatalf("deployment %+v", dep)
	}
	installed := 0
	for name := range s.files {
		if strings.HasPrefix(name, "migrations/") {
			installed++
		}
	}
	if n := s.c.Count(`SELECT count(*) FROM core.schema_migrations WHERE owner = 'mod_quickbooks' AND version > 0`); n != installed || n < 3 {
		t.Fatalf("%d migrations recorded, the package installs %d", n, installed)
	}
	mod := s.c.Admin.Must(http.StatusOK, "GET", "/api/v1/modules/"+id, nil)
	perms, _ := mod.JSON["permissions"].([]any)
	for _, p := range perms {
		if m := p.(map[string]any); m["approved"] != true {
			t.Fatalf("scope not approved: %v", m)
		}
	}
	// A fresh install waits for the admin's decisions on the Modules tab, which lists every scope.
	s.c.Uninstall(id, true)
	s.c.Install(s.signed)
	page := s.c.Admin.Must(http.StatusOK, "GET", "/admin?tab=modules", nil)
	for _, p := range perms {
		if scope := p.(map[string]any)["scope"].(string); !strings.Contains(page.Body, "<code>"+scope+"</code>") {
			t.Fatalf("the Modules tab lacks the scope %s", scope)
		}
	}
	for _, line := range strings.Split(string(s.files["togavsu-module.yaml"]), "\n") {
		if reason, ok := strings.CutPrefix(line, "    reason: "); ok && !strings.Contains(page.Body, html.EscapeString(reason)) {
			s.missing[scopeReasons] = true
		}
	}

	s.needs(core02, core06, scopeReasons)
	if len(perms) != 10 {
		t.Fatalf("%d scopes", len(perms))
	}
	// Declining books:read cancels the install and creates nothing.
	var decisions []map[string]string
	for _, p := range perms {
		scope := p.(map[string]any)["scope"].(string)
		d := "approve"
		if scope == "books:read" {
			d = "decline"
		}
		decisions = append(decisions, map[string]string{"scope": scope, "decision": d})
	}
	s.c.Admin.Do("POST", "/api/v1/modules/"+id+"/permissions", map[string]any{"decisions": decisions})
	if s.c.SchemaExists(id) {
		t.Fatal("the schema exists after a declined install")
	}
	if r := s.c.Admin.Do("GET", "/api/v1/modules/"+id, nil); r.Code == http.StatusOK && r.JSON["status"] == "active" {
		t.Fatalf("the module is active after a declined install: %s", r.Body)
	}
}

func TestMODQBO02ForeignKeys(t *testing.T) {
	s := newSuite(t)
	rows, err := s.db.Query(`SELECT k.conname, k.confdeltype, f.relnamespace::regnamespace::text FROM pg_constraint k
JOIN pg_class f ON f.oid = k.confrelid WHERE k.contype = 'f' AND k.connamespace = current_schema()::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	coreLinks := 0
	for rows.Next() {
		var name, del, ns string
		if err := rows.Scan(&name, &del, &ns); err != nil {
			t.Fatal(err)
		}
		if ns == "core" {
			coreLinks++
			if del != "r" {
				t.Fatalf("%s is not ON DELETE RESTRICT (%s)", name, del)
			}
		}
	}
	rows.Close()
	want := 0
	for name, b := range s.files {
		if strings.HasPrefix(name, "migrations/") {
			want += strings.Count(string(b), "REFERENCES core.")
		}
	}
	if coreLinks != want || want < 16 {
		t.Fatalf("%d core foreign keys, the migrations declare %d", coreLinks, want)
	}

	// Hard-deleting a contact the module holds is refused by the module's foreign key.
	contact := str(t, s.c.Admin.Must(http.StatusCreated, "POST", "/api/v1/contacts", map[string]any{"kind": "person", "given_name": "Hold", "family_name": "Fast"}).JSON, "id")
	if _, err := s.db.Exec(`INSERT INTO customer_sync (id, contact_id, state, next_attempt_at, updated_at) VALUES ('held-customer', $1, 'synced', 0, 0)`, contact); err != nil {
		t.Fatal(err)
	}
	coreDB, err := sql.Open("pgx", s.c.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer coreDB.Close()
	_, err = coreDB.Exec(`DELETE FROM core.contacts WHERE id = $1`, contact)
	if err == nil || !strings.Contains(err.Error(), "customer_sync") {
		t.Fatalf("hard delete: %v", err)
	}

	s.needs(core22) // the trust bank map's link to core.trust_accounts
}

func TestMODQBO03Widget(t *testing.T) {
	s := newSuite(t)
	if !strings.Contains(string(s.files["togavsu-module.yaml"]), "    target:\n      tab: books\n      params: [status]") {
		t.Fatal("widget target is not the books tab")
	}
	s.needs(pushes...)
	s.configure(nil)
	if r := s.connect(realmA); r.Code != http.StatusOK {
		t.Fatalf("connect %d %s", r.Code, r.Body)
	}
	s.sentInvoice(s.c.Open, 4, 25000)
	s.deliver()
	w := decode[quickbooks.WidgetView](t, s.get(coretest.Accountant, "/api/status", s.ctxFor(coretest.Accountant, "")))
	if w.Counts.Synced != 1 {
		t.Fatalf("counts %+v", w.Counts)
	}
	for _, k := range []string{"synced", "pending", "exceptions"} {
		if w.Links[k] != "/modules/quickbooks/tabs/books?status="+k {
			t.Fatalf("link %s = %q", k, w.Links[k])
		}
	}
	page := s.get(coretest.Admin, "/widget", s.ctxFor(coretest.Admin, ""))
	if page.Code != http.StatusOK || !strings.Contains(page.Body, `href="/modules/quickbooks/tabs/books?status=exceptions"`) {
		t.Fatalf("widget page %d %s", page.Code, page.Body)
	}
	if r := s.get(coretest.Staff, "/widget", s.ctxFor(coretest.Staff, "")); r.Code != http.StatusNotFound {
		t.Fatalf("widget for staff %d", r.Code)
	}
}

func TestMODQBO04OAuth(t *testing.T) {
	s := newSuite(t)
	// A non-admin cannot store the client secret, and a made-up state is refused.
	for _, who := range []string{coretest.Accountant, coretest.Attorney, coretest.Staff} {
		if r := s.act(who, "client-secret", s.ctxFor(who, ""), map[string]string{"client_secret": "x"}); r.Code != http.StatusForbidden {
			t.Fatalf("client secret as %s: %d", who, r.Code)
		}
	}
	browser := s.c.NewClient()
	if r := browser.Do("GET", "/m/"+id+"/oauth/callback?"+url.Values{"state": {"made-up"}, "code": {"x"}, "realmId": {realmA}}.Encode(), nil); r.Code != http.StatusForbidden {
		t.Fatalf("made-up state %d", r.Code)
	}

	s.needs(pushes...)
	s.configure(nil)
	admin := s.ctxFor(coretest.Admin, "")
	s.mustAct(coretest.Admin, "client-secret", map[string]string{"client_secret": s.clientSecret})
	setupPage := s.get(coretest.Admin, "/setup", admin)
	if !strings.Contains(setupPage.Body, `target="_blank" rel="noopener" data-connect`) {
		t.Fatalf("setup page %s", setupPage.Body)
	}
	setup := decode[quickbooks.SetupView](t, s.get(coretest.Admin, "/api/setup", admin))
	start := browser.Do("GET", "/m/"+id+"/"+setup.ConnectURL, nil)
	loc, _ := url.Parse(start.Loc)
	if start.Code != http.StatusFound || loc == nil || loc.Host != "appcenter.intuit.com" {
		t.Fatalf("start %d %s", start.Code, start.Loc)
	}
	state := loc.Query().Get("state")
	cb := browser.Do("GET", "/m/"+id+"/oauth/callback?"+url.Values{"state": {state}, "code": {s.qb.IssueCode(realmA)}, "realmId": {realmA}}.Encode(), nil)
	if cb.Code != http.StatusOK {
		t.Fatalf("callback %d %s", cb.Code, cb.Body)
	}
	mod := s.c.ModuleClient(id)
	secret := func(name string) string {
		r := mod.Do("GET", "/api/v1/modules/"+id+"/secrets/"+name, nil)
		v, _ := r.JSON["value"].(string)
		return v
	}
	access, refresh := secret("qbo/access_token"), secret("qbo/refresh_token")
	if access == "" || refresh == "" || !s.qb.AccessValid(access) {
		t.Fatal("the tokens are not in the core's secret store")
	}
	if r := browser.Do("GET", "/m/"+id+"/oauth/callback?"+url.Values{"state": {state}, "code": {s.qb.IssueCode(realmA)}}.Encode(), nil); r.Code != http.StatusForbidden {
		t.Fatalf("reused state %d", r.Code)
	}
	setup = decode[quickbooks.SetupView](t, s.get(coretest.Admin, "/api/setup", admin))
	s.clock.Advance(11 * time.Minute)
	if r := browser.Do("GET", "/m/"+id+"/"+setup.ConnectURL, nil); r.Code != http.StatusForbidden {
		t.Fatalf("expired state %d", r.Code)
	}
	if v := decode[quickbooks.SetupView](t, s.get(coretest.Accountant, "/api/setup", s.ctxFor(coretest.Accountant, ""))); v.ConnectURL != "" {
		t.Fatal("the accountant got a connect link")
	}
	var responses strings.Builder
	for _, p := range []string{"/api/setup", "/setup", "/api/books", "/books", "/api/status", "/widget", "/api/openapi.json"} {
		responses.WriteString(s.get(coretest.Admin, p, s.ctxFor(coretest.Admin, "")).Body)
	}
	for name, hay := range map[string]string{"module tables": s.dump(), "log": s.logs.String(), "responses": responses.String() + cb.Body} {
		for _, v := range []string{access, refresh, s.clientSecret} {
			if strings.Contains(hay, v) {
				t.Fatalf("the %s hold a token or the client secret", name)
			}
		}
	}

	s.needs(core09) // Connect opens its own top-level window from the frame
}

func TestMODQBO05Refresh(t *testing.T) {
	s := newSuite(t)
	s.needs(append(pushes, core03)...)
	s.configure(nil)
	if r := s.connect(realmA); r.Code != http.StatusOK {
		t.Fatalf("connect %d %s", r.Code, r.Body)
	}
	mod := s.c.ModuleClient(id)
	refreshToken := func() string {
		v, _ := mod.Do("GET", "/api/v1/modules/"+id+"/secrets/qbo/refresh_token", nil).JSON["value"].(string)
		return v
	}
	old := refreshToken()
	inv := s.sentInvoice(s.c.Open, 2, 10000)
	s.c.DeliverEvents() // before the clock moves past the webhook skew
	s.clock.Advance(2 * time.Hour)
	s.tick()
	if s.invoiceRow(inv).State != "synced" {
		t.Fatal("the push after expiry failed")
	}
	if nr := refreshToken(); nr == old || !s.qb.RefreshValid(nr) || s.qb.RefreshValid(old) {
		t.Fatal("the rotated refresh token was not stored")
	}
	s.qb.RevokeRefresh()
	s.clock.Set(time.Now().UTC()) // the core signs webhooks with its own clock
	inv2 := s.sentInvoice(s.c.Open, 2, 10000)
	s.c.DeliverEvents()
	s.clock.Advance(2 * time.Hour)
	s.tick()
	setup := decode[quickbooks.SetupView](t, s.get(coretest.Admin, "/api/setup", s.ctxFor(coretest.Admin, "")))
	if setup.Connection.State != "reconnect_needed" {
		t.Fatalf("state %s", setup.Connection.State)
	}
	if r := s.invoiceRow(inv2); r.State != "pending" || r.Attempts != 0 {
		t.Fatalf("paused row %+v", r)
	}
	if len(s.notifications(coretest.Admin, "quickbooks.sync_problem")) == 0 {
		t.Fatal("the admin got no alert")
	}
	if len(s.notifications(coretest.Accountant, "quickbooks.sync_problem")) != 0 {
		t.Fatal("a connection alert went to the accountant")
	}
}

func TestMODQBO29Purchases(t *testing.T) {
	s := newConnected(t)
	lines := s.expense(s.c.Open, "Court reporter", 32000, true)
	s.needsRow("expenses", lines)
	s.deliver()
	if r := s.expenseRow(lines); r.State != "skipped" || len(s.entities(qbo.KindPurchase)) != 0 {
		t.Fatalf("invoice_lines_only %+v", r)
	}
	s.setting(map[string]string{"expense_push": "purchases", "account_expense_paid_from": "Firm Card"})
	x := s.expense(s.c.Open, "Courier", 4500, false)
	s.deliver()
	s.tick()
	ps := s.entities(qbo.KindPurchase)
	if len(ps) != 1 {
		t.Fatalf("%d purchases", len(ps))
	}
	p := ps[0].(*qbo.Purchase)
	d := p.Line[0].AccountBasedExpenseLineDetail
	if p.AccountRef.Value != "41" || d.AccountRef.Value != "91" || d.BillableStatus != "NotBillable" || d.CustomerRef.Value != s.customerID(s.c.ClientContact) {
		t.Fatalf("purchase %+v %+v", p, d)
	}
	if s.expenseRow(x).State != "synced" {
		t.Fatal("expense not synced")
	}
}

func TestMODQBO30Sandbox(t *testing.T) {
	s := newConnected(t)
	s.tick()
	if got := s.qb.EnvironmentSeen(realmA); got != "sandbox" {
		t.Fatalf("environment %q", got)
	}
	admin := s.ctxFor(coretest.Admin, s.c.Open)
	for _, p := range []string{"/widget", "/books", "/setup", "/matter"} {
		if r := s.get(coretest.Admin, p, admin); r.Code != http.StatusOK || !strings.Contains(r.Body, `data-banner="sandbox"`) {
			t.Fatalf("%s: %d banner missing", p, r.Code)
		}
	}
	if w := decode[quickbooks.WidgetView](t, s.get(coretest.Admin, "/api/status", admin)); !w.Sandbox {
		t.Fatal("the widget does not say Sandbox")
	}
	s.setting(map[string]string{"qbo_environment": "production"})
	if r := s.get(coretest.Admin, "/books", admin); strings.Contains(r.Body, `data-banner="sandbox"`) {
		t.Fatal("banner in production")
	}
}

func TestMODQBO31RealmSwitch(t *testing.T) {
	s := newConnected(t)
	s.sentInvoice(s.c.Open, 2, 10000)
	s.deliver()
	logs := s.count(`SELECT count(*) FROM sync_log`)
	s.mustAct(coretest.Admin, "disconnect", nil)
	if r := s.c.ModuleClient(id).Do("GET", "/api/v1/modules/"+id+"/secrets/qbo/access_token", nil); r.Code != http.StatusNotFound {
		t.Fatalf("tokens kept after disconnect: %d", r.Code)
	}
	if r := s.connect(realmB); r.Code != http.StatusConflict {
		t.Fatalf("realm switch %d %s", r.Code, r.Body)
	}
	if r := s.act(coretest.Admin, "reset", s.ctxFor(coretest.Admin, ""), map[string]string{"confirm": "reset"}); r.Code != http.StatusBadRequest {
		t.Fatalf("reset without RESET %d", r.Code)
	}
	s.mustAct(coretest.Admin, "reset", map[string]string{"confirm": "RESET"})
	for _, tb := range []string{"customer_sync", "invoice_sync", "payment_sync", "trust_entry_sync", "expense_sync"} {
		if n := s.count(`SELECT count(*) FROM ` + tb); n != 0 {
			t.Fatalf("%s holds %d rows", tb, n)
		}
	}
	if n := s.count(`SELECT count(*) FROM sync_log`); n != logs {
		t.Fatalf("sync_log %d, was %d", n, logs)
	}
	if r := s.connect(realmB); r.Code != http.StatusOK {
		t.Fatalf("connect after reset %d", r.Code)
	}
}

func TestMODQBO32Include(t *testing.T) {
	s := newSuite(t)
	s.timeEntry(s.c.Open, 2, 10000)
	inv := s.draft(s.c.Open) // skips while the core keeps invoices out of core.invoices
	s.send(inv)
	if _, err := s.db.Exec(`INSERT INTO invoice_sync (invoice_id, state, next_attempt_at, updated_at, synced_at) VALUES ($1, 'synced', 0, 0, 0)`, inv); err != nil {
		t.Fatal(err)
	}
	r := s.c.Accountant.Must(http.StatusOK, "GET", "/api/v1/invoices/"+inv+"?include=quickbooks.invoice_sync", nil)
	included, _ := r.JSON["included"].(map[string]any)
	rows, _ := included["quickbooks.invoice_sync"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["state"] != "synced" {
		t.Fatalf("included %v", r.JSON["included"])
	}
	walled, _ := s.matter("Estate of Wallace")
	s.c.Wall(walled, s.c.Attorney)
	s.timeEntry(walled, 2, 10000)
	hidden := s.draft(walled)
	if r := s.c.Accountant.Do("GET", "/api/v1/invoices/"+hidden+"?include=quickbooks.invoice_sync", nil); r.Code != http.StatusNotFound {
		t.Fatalf("walled invoice for the accountant: %d", r.Code)
	}
}

func TestMODQBO33TotalsCheck(t *testing.T) {
	s := newConnected(t)
	dep := s.postTrust(s.c.Open, "deposit", 150000, "Retainer")
	s.deliver()
	s.qb.AlterJournalEntry(realmA, s.trustRow(dep).QBOID, 140000)
	s.syncNow()
	r := s.trustRow(dep)
	if r.State != "drift" || r.Code != "totals_mismatch" || !strings.Contains(r.Error, s.trustAccount()) || !strings.Contains(r.Error, "-100.00") {
		t.Fatalf("row %+v", r)
	}
}

func TestMODQBO34TunableSettings(t *testing.T) {
	s := newSuite(t)
	got := map[string]string{}
	var rows []map[string]any
	r := s.c.Admin.Must(http.StatusOK, "GET", "/api/v1/modules/"+id+"/settings", nil)
	if err := jsonInto(r.Body, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		k, _ := row["key"].(string)
		v, _ := row["default"].(string)
		got[k] = v
	}
	if got["retry_schedule"] != "1m,5m,30m,2h,6h" || got["alert_interval_minutes"] != "15" {
		t.Fatalf("settings defaults %v", got)
	}
	s.needs(append(pushes, core03)...)
	s.configure(map[string]string{"retry_schedule": "3m,7m"})
	if r := s.connect(realmA); r.Code != http.StatusOK {
		t.Fatalf("connect %d", r.Code)
	}
	inv := s.sentInvoice(s.c.Open, 2, 10000)
	s.c.DeliverEvents()
	s.qb.FailNext(qbo.KindInvoice, unavailable())
	s.tick()
	if r := s.invoiceRow(inv); r.NextAttemptAt != s.clock.Now().Add(3*time.Minute).UnixMicro() {
		t.Fatalf("next attempt %+v", r)
	}
	s.qb.FailNext(qbo.KindInvoice, unavailable())
	s.clock.Advance(3 * time.Minute)
	s.tick()
	if r := s.invoiceRow(inv); r.State != "failed" {
		t.Fatalf("two-step schedule %+v", r)
	}
	if n := len(s.notifications(coretest.Admin, "quickbooks.sync_problem")); n != 1 {
		t.Fatalf("%d alerts", n)
	}
}
