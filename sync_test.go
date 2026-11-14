package quickbooks_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	quickbooks "github.com/CharitonMedia/togavsu-quickbooks"
	"github.com/CharitonMedia/togavsu-quickbooks/internal/fakecore"
	"github.com/CharitonMedia/togavsu-quickbooks/internal/wire"
	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
)

func unavailable() *qbo.Fault {
	return &qbo.Fault{HTTPStatus: 503, Code: "503", Message: "Service unavailable", Retryable: true}
}

func TestMODQBO19DuplicateEvents(t *testing.T) {
	e := newEnv(t)
	inv := e.sentInvoice(e.matter, "INV-7001", 10000) // invoice.created and invoice.sent coalesce
	ev := e.core.Delivered()[len(e.core.Delivered())-1]
	if code := e.core.Deliver(ev); code != http.StatusNoContent {
		t.Fatalf("redelivery %d", code)
	}
	if n := e.count(`SELECT count(*) FROM received_events WHERE event_id = $1`, ev.ID); n != 1 {
		t.Fatalf("recorded %d times", n)
	}
	e.tick()
	e.tick()
	creates := 0
	for _, c := range e.qb.Writes() {
		if c.Op == "create" && c.Kind == qbo.KindInvoice {
			creates++
		}
	}
	if creates != 1 || e.invoiceRow(inv).State != "synced" {
		t.Fatalf("%d invoice creates", creates)
	}
}

// TestEventTakenAgainAfterFailedQueue: an event the module could not queue is not
// recorded, so the core's redelivery is taken and queues the row.
func TestEventTakenAgainAfterFailedQueue(t *testing.T) {
	e := newEnv(t)
	ev := wire.EventPayload{ID: "evt-late-invoice", Action: "invoice.sent", RecordType: "invoice", RecordID: "inv-not-yet", OccurredAt: e.clock.Now().Format(time.RFC3339)}
	if code := e.core.Deliver(ev); code != http.StatusInternalServerError {
		t.Fatalf("an event for a record the core table lacks answered %d", code)
	}
	if n := e.count(`SELECT count(*) FROM received_events WHERE event_id = $1`, ev.ID); n != 0 {
		t.Fatal("the failed event was recorded")
	}
	if _, err := e.db.Exec(`INSERT INTO core.invoices (id) VALUES ($1)`, ev.RecordID); err != nil {
		t.Fatal(err)
	}
	if code := e.core.Deliver(ev); code != http.StatusNoContent {
		t.Fatalf("redelivery %d", code)
	}
	if r := e.invoiceRow(ev.RecordID); r.State != "pending" {
		t.Fatalf("row %+v", r)
	}
}

func TestMODQBO20RetryThenFail(t *testing.T) {
	e := newEnv(t)
	e.core.SetEmail(e.accountant, false)
	inv := e.sentInvoice(e.matter, "INV-7101", 10000)
	e.qb.FailNext(qbo.KindInvoice, unavailable())
	e.tick()
	r := e.invoiceRow(inv)
	if r.State != "pending" || r.Attempts != 1 || r.NextAttemptAt != e.clock.Now().Add(time.Minute).UnixMicro() {
		t.Fatalf("after one 503 %+v", r)
	}
	e.clock.Advance(time.Minute)
	e.tick()
	if r := e.invoiceRow(inv); r.State != "synced" {
		t.Fatalf("retry %+v", r)
	}

	inv2 := e.sentInvoice(e.matter, "INV-7102", 20000)
	waits := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	for i := 0; i < 5; i++ {
		e.qb.FailNext(qbo.KindInvoice, unavailable())
		e.tick()
		if i < 4 {
			e.clock.Advance(waits[i])
		}
	}
	r = e.invoiceRow(inv2)
	if r.State != "failed" || r.Attempts != 5 || r.Code != "503" {
		t.Fatalf("after five 503s %+v", r)
	}
	var alerts []fakecore.Notification
	for _, n := range e.core.Notifications {
		if n.Link == "/modules/quickbooks/tabs/books?status=exceptions" {
			alerts = append(alerts, n)
		}
	}
	if len(alerts) != 1 || alerts[0].Kind != "quickbooks.sync_problem" || len(alerts[0].Recipients) != 2 {
		t.Fatalf("alerts %+v", alerts)
	}
	if !equal(alerts[0].Emailed, []string{e.admin}) {
		t.Fatalf("emailed %v", alerts[0].Emailed)
	}
	e.mustAct("retry", e.ctxFor(e.accountant, ""), map[string]string{"kind": "invoice", "id": inv2})
	if r := e.invoiceRow(inv2); r.State != "synced" {
		t.Fatalf("after Retry %+v", r)
	}
	if n := e.count(`SELECT count(*) FROM sync_log WHERE invoice_id = $1 AND operation = 'retry_requested' AND actor_user_id = $2`, inv2, e.accountant); n != 1 {
		t.Fatalf("retry log rows %d", n)
	}
}

func TestMODQBO21TimeoutReplay(t *testing.T) {
	e := newEnv(t)
	inv := e.sentInvoice(e.matter, "INV-7201", 10000)
	e.qb.Timeout(qbo.KindInvoice)
	e.tick()
	r := e.invoiceRow(inv)
	if r.State != "pending" || r.RequestID == "" || r.QBOID != "" {
		t.Fatalf("after timeout %+v", r)
	}
	e.clock.Advance(time.Minute)
	e.tick()
	r2 := e.invoiceRow(inv)
	if r2.State != "synced" || len(e.entities(qbo.KindInvoice)) != 1 {
		t.Fatalf("after retry %+v, %d invoices", r2, len(e.entities(qbo.KindInvoice)))
	}
	replayed := false
	for _, c := range e.qb.Calls() {
		if c.Kind == qbo.KindInvoice && c.Op == "create" && c.Replayed && c.RequestID == r.RequestID {
			replayed = true
		}
	}
	if !replayed {
		t.Fatal("the retry did not reuse the requestid")
	}
}

func TestMODQBO22MappingInvalid(t *testing.T) {
	e := newEnv(t)
	e.core.Settings["account_trust_liability"] = "Legal Fee Income"
	d := e.core.PostTrust(e.matter, e.trust, "deposit", 10000, "Retainer")
	w := e.core.PostTrust(e.matter, e.trust, "withdrawal", 1000, "Costs")
	e.tick()
	for _, id := range []string{d, w} {
		if r := e.trustRow(id); r.State != "failed" || r.Code != "mapping_invalid" || r.Attempts != 0 {
			t.Fatalf("row %+v", r)
		}
	}
	if n := len(e.entities(qbo.KindJournalEntry)); n != 0 {
		t.Fatalf("%d journal entries posted", n)
	}
	e.core.Settings["account_trust_liability"] = "Client Trust Liability"
	e.tick()
	for _, id := range []string{d, w} {
		if r := e.trustRow(id); r.State != "synced" {
			t.Fatalf("after the fix %+v", r)
		}
	}
}

func TestMODQBO23NoCoreWrites(t *testing.T) {
	e := newEnv(t)
	for _, s := range e.core.Granted {
		if !strings.HasSuffix(s, ":read") && s != "notifications:write" && s != "module_secrets:write" {
			t.Fatalf("write scope %s", s)
		}
	}
	inv := e.sentInvoice(e.matter, "INV-7301", 10000)
	e.tick()
	before := e.core.Invoice(inv)
	for _, tok := range []string{"", e.ctxFor(e.accountant, "")} {
		for _, call := range []struct{ method, path string }{
			{http.MethodPost, "/api/v1/payments"}, {http.MethodPost, "/api/v1/trust-ledger"},
			{http.MethodPatch, "/api/v1/invoices/" + inv}, {http.MethodPost, "/api/v1/invoices/" + inv + "/void"},
		} {
			status := coreCall(t, e, call.method, call.path, tok, `{"amount_cents":1}`)
			if status != http.StatusForbidden {
				t.Fatalf("%s %s (%q): %d", call.method, call.path, tok, status)
			}
		}
	}
	mark := len(e.core.Requests)
	e.qb.EditAsAccountant(realmA, qbo.KindInvoice, e.invoiceRow(inv).QBOID)
	e.syncNow()
	acct := e.ctxFor(e.accountant, "")
	e.mustAct("repush", acct, map[string]string{"kind": "invoice", "id": inv})
	e.qb.EditAsAccountant(realmA, qbo.KindInvoice, e.invoiceRow(inv).QBOID)
	e.clock.Advance(time.Minute)
	e.syncNow()
	e.mustAct("acknowledge", acct, map[string]string{"kind": "invoice", "id": inv})
	if after := e.core.Invoice(inv); after.TotalCents != before.TotalCents || after.Status != before.Status || after.AmountPaidCents != before.AmountPaidCents {
		t.Fatalf("core invoice changed: %+v", after)
	}
	for _, req := range e.core.Requests[mark:] {
		if !strings.HasPrefix(req, "GET ") && !strings.Contains(req, "/notifications") && !strings.Contains(req, "/secrets/") && !strings.Contains(req, "/context-tokens/verify") {
			t.Fatalf("the module made a core write: %s", req)
		}
	}
}

func coreCall(t *testing.T, e *env, method, path, tok, body string) int {
	t.Helper()
	req, _ := http.NewRequest(method, e.core.URL()+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.core.Token)
	if tok != "" {
		req.Header.Set("X-Togavsu-Context-Token", tok)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestMODQBO24Roles(t *testing.T) {
	e := newEnv(t)
	inv := e.sentInvoice(e.matter, "INV-7401", 10000)
	e.qb.FailNext(qbo.KindInvoice, &qbo.Fault{HTTPStatus: 400, Code: "6000", Message: "business rule"})
	e.tick()
	for _, u := range []string{e.staff, e.attorney} {
		tok := e.ctxFor(u, "")
		for _, a := range []string{"retry", "acknowledge", "sync-now", "reset", "disconnect", "create-missing"} {
			if r := e.act(a, tok, map[string]string{"kind": "invoice", "id": inv, "confirm": "RESET"}); r.status != http.StatusForbidden {
				t.Fatalf("%s as %s: %d", a, u, r.status)
			}
		}
		if r := e.get("/api/setup", tok); r.status != http.StatusNotFound {
			t.Fatalf("setup as %s: %d", u, r.status)
		}
	}
	acct := e.ctxFor(e.accountant, "")
	for _, a := range []string{"disconnect", "create-missing", "reset", "client-secret", "trust-bank-map"} {
		if r := e.act(a, acct, map[string]string{"confirm": "RESET", "client_secret": "x"}); r.status != http.StatusForbidden {
			t.Fatalf("%s as accountant: %d", a, r.status)
		}
	}
	if v := decode[quickbooks.SetupView](t, e.get("/api/setup", acct)); v.ConnectURL != "" || v.CanAct {
		t.Fatalf("accountant setup %+v", v)
	}
	if r := e.invoiceRow(inv); r.State != "failed" {
		t.Fatalf("row changed %+v", r)
	}
	admin := e.ctxFor(e.admin, "")
	e.mustAct("create-missing", admin, nil)
	e.mustAct("retry", acct, map[string]string{"kind": "invoice", "id": inv})
	e.mustAct("reset", admin, map[string]string{"confirm": "RESET"})
	e.mustAct("disconnect", admin, nil)
}

func TestMODQBO25NightlyFindsMissedEvents(t *testing.T) {
	e := newEnv(t)
	e.core.Muted = true
	inv := e.sentInvoice(e.matter, "INV-7501", 10000)
	dep := e.core.PostTrust(e.matter, e.trust, "deposit", 5000, "Retainer")
	e.core.Muted = false
	e.tick()
	if _, ok := e.row("invoice_sync", "invoice_id", inv); ok {
		t.Fatal("a row exists with no event")
	}
	e.clock.Advance(17 * time.Hour) // 08:00 UTC next day, the reconcile hour
	e.tick()
	if e.invoiceRow(inv).State != "synced" || e.trustRow(dep).State != "synced" {
		t.Fatal("the nightly run did not push the missed records")
	}
	before := e.writes()
	e.syncNow()
	if after := e.writes(); after != before {
		t.Fatalf("a second run with no changes made %d writes", after-before)
	}
}

func TestMODQBO26Drift(t *testing.T) {
	e := newEnv(t)
	inv := e.sentInvoice(e.matter, "INV-7601", 10000)
	e.tick()
	id := e.invoiceRow(inv).QBOID
	pushed := e.qb.Get(realmA, qbo.KindInvoice, id).(*qbo.Invoice)
	e.qb.EditAsAccountant(realmA, qbo.KindInvoice, id)
	e.clock.Advance(time.Minute)
	e.syncNow()
	if r := e.invoiceRow(inv); r.State != "drift" {
		t.Fatalf("after the accountant edit %+v", r)
	}
	books := decode[quickbooks.BooksView](t, e.get("/api/books?status=exceptions", e.ctxFor(e.accountant, "")))
	if len(books.Rows) != 1 || !books.Rows[0].CanDrift {
		t.Fatalf("exceptions %+v", books.Rows)
	}
	e.mustAct("repush", e.ctxFor(e.accountant, ""), map[string]string{"kind": "invoice", "id": inv})
	got := e.qb.Get(realmA, qbo.KindInvoice, id).(*qbo.Invoice)
	if e.invoiceRow(inv).State != "synced" || got.Line[0].Description != pushed.Line[0].Description || *got.TotalAmt != *pushed.TotalAmt {
		t.Fatalf("re-push %+v", got)
	}
	e.qb.EditAsAccountant(realmA, qbo.KindInvoice, id)
	e.clock.Advance(time.Minute)
	e.syncNow()
	e.mustAct("acknowledge", e.ctxFor(e.accountant, ""), map[string]string{"kind": "invoice", "id": inv})
	e.clock.Advance(time.Minute)
	e.syncNow()
	if r := e.invoiceRow(inv); r.State != "acknowledged" {
		t.Fatalf("acknowledged row came back %+v", r)
	}
	e.core.EditInvoice(inv, func(i *quickbooks.Invoice) {
		i.Lines = append(i.Lines, quickbooks.InvoiceLine{Kind: "time", Description: "Follow-up call", AmountCents: 500})
		i.TotalCents += 500
	})
	e.tick()
	if r := e.invoiceRow(inv); r.State != "drift" {
		t.Fatalf("after a Togavsu change %+v", r)
	}
}

func TestMODQBO27Customers(t *testing.T) {
	e := newEnv(t)
	e.core.PostTrust(e.matter, e.trust, "deposit", 1000, "Retainer")
	existing := e.qb.AddCustomer(realmA, qbo.Customer{DisplayName: "Morgan Existing"})
	morgan := e.core.AddContact(quickbooks.BooksContact{GivenName: "Morgan", FamilyName: "Existing"})
	e.core.PostTrust(e.core.AddMatter("2026-0050", "Existing matter", morgan), e.trust, "deposit", 1000, "Retainer")
	e.qb.AddVendor(realmA, "Vic Vendor")
	vic := e.core.AddContact(quickbooks.BooksContact{GivenName: "Vic", FamilyName: "Vendor"})
	e.core.PostTrust(e.core.AddMatter("2026-0051", "Vendor matter", vic), e.trust, "deposit", 1000, "Retainer")
	e.tick()
	jane := e.qb.Get(realmA, qbo.KindCustomer, e.customerID(e.jane)).(*qbo.Customer)
	if jane.DisplayName != "Jane Sample" || !strings.Contains(jane.Notes, "Togavsu contact "+e.jane) {
		t.Fatalf("jane %+v", jane)
	}
	if e.customerID(morgan) != existing || e.count(`SELECT count(*) FROM sync_log WHERE contact_id = $1 AND operation = 'adopt'`, morgan) != 1 {
		t.Fatal("Morgan was not adopted")
	}
	v := e.qb.Get(realmA, qbo.KindCustomer, e.customerID(vic)).(*qbo.Customer)
	if v.DisplayName != "Vic Vendor ("+strings.ReplaceAll(vic, "-", "")[:8]+")" {
		t.Fatalf("vendor name %q", v.DisplayName)
	}
	e.core.UpdateContact(e.jane, func(c *quickbooks.BooksContact) {
		c.FamilyName = "Sample-Example"
		c.BillingAddress = &quickbooks.BooksAddress{Line1: "2 New Road", City: "Springfield", Region: "IL", PostalCode: "62702", Country: "US"}
	})
	e.tick()
	jane = e.qb.Get(realmA, qbo.KindCustomer, e.customerID(e.jane)).(*qbo.Customer)
	if jane.DisplayName != "Jane Sample-Example" || jane.BillAddr == nil || jane.BillAddr.Line1 != "2 New Road" {
		t.Fatalf("after contact.updated %+v", jane)
	}
}

func TestTrustBankMap(t *testing.T) {
	e := newEnv(t)
	second := e.core.AddTrustAccount("Second Trust")
	admin := e.ctxFor(e.admin, "")
	e.mustAct("trust-bank-map", admin, map[string]string{"trust_account_id": second, "qbo_account": "Second Trust"})
	setup := decode[quickbooks.SetupView](t, e.get("/api/setup", admin))
	found := false
	for _, b := range setup.TrustBanks {
		found = found || (b.TrustAccountID == second && b.QBOAccount == "Second Trust")
	}
	if !found {
		t.Fatalf("map %+v", setup.TrustBanks)
	}
	a := e.core.PostTrust(e.matter, e.trust, "deposit", 1000, "Into the default trust bank")
	b := e.core.PostTrust(e.matter, second, "deposit", 2000, "Into the second trust bank")
	e.tick()
	bal := e.qb.Balances(realmA)
	if bal["36"] != 1000 || bal["37"] != 2000 || e.trustRow(a).State != "synced" || e.trustRow(b).State != "synced" {
		t.Fatalf("balances %v", bal)
	}
	// a mapped account that is not a Bank stops trust posting with mapping_invalid
	e.mustAct("trust-bank-map", admin, map[string]string{"trust_account_id": second, "qbo_account": "Legal Fee Income"})
	c := e.core.PostTrust(e.matter, e.trust, "deposit", 500, "Blocked by the map")
	e.tick()
	if r := e.trustRow(c); r.State != "failed" || r.Code != "mapping_invalid" {
		t.Fatalf("row %+v", r)
	}
	if r := e.act("trust-bank-map", admin, map[string]string{"trust_account_id": "no-such-account", "qbo_account": "Second Trust"}); r.status != http.StatusBadRequest {
		t.Fatalf("unknown trust account %d", r.status)
	}
	w := decode[quickbooks.WidgetView](t, e.get("/api/status", admin))
	if w.Counts.SyncedToday != 2 {
		t.Fatalf("synced today %+v", w.Counts)
	}
	if r := e.get("/api/matter", e.ctxFor(e.accountant, e.matter)); r.status != http.StatusNotFound {
		t.Fatalf("matter tab for the accountant %d", r.status)
	}
	m := decode[quickbooks.MatterView](t, e.get("/api/matter", e.ctxFor(e.staff, e.matter)))
	if m.Matter.Number != "2026-0042" || len(m.Lines) != 3 {
		t.Fatalf("matter tab %+v", m)
	}
}
