package acceptance_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	quickbooks "github.com/CharitonMedia/togavsu-quickbooks"
	"github.com/CharitonMedia/togavsu-quickbooks/internal/wire"
	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
	"github.com/CharitonMedia/togavsu/pkg/modulekit/coretest"
)

func TestMODQBO19DuplicateEvents(t *testing.T) {
	s := newSuite(t)
	// The core's own signed webhooks reach /events and each is recorded once.
	s.timeEntry(s.c.Open, 2, 10000)
	draft := s.c.Attorney.Must(http.StatusCreated, "POST", "/api/v1/invoice-drafts", map[string]any{"matter_id": s.c.Open})
	inv := str(t, draft.JSON["data"].([]any)[0].(map[string]any), "id")
	s.send(inv)
	s.needsRow("invoices", inv) // an invoice event queues a row whose key links to core.invoices
	s.c.DeliverEvents()
	if n := s.count(`SELECT count(*) FROM received_events WHERE record_id = $1`, inv); n != 3 {
		t.Fatalf("%d events recorded for the invoice", n)
	}
	// The same event delivered again is recorded once.
	var ev wire.EventPayload
	if err := s.db.QueryRow(`SELECT event_id, type, record_id FROM received_events WHERE type = 'invoice.sent'`).Scan(&ev.ID, &ev.Action, &ev.RecordID); err != nil {
		t.Fatal(err)
	}
	ev.RecordType = "invoice"
	body, _ := json.Marshal(ev)
	replay := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(body))
	replay.Header.Set(wire.SignatureHeader, wire.SignWebhook(s.c.Last(id).Secrets["togavsu_webhook_secret"], time.Now(), body))
	rec := httptest.NewRecorder()
	s.server().Handler().ServeHTTP(rec, replay)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("redelivery %d %s", rec.Code, rec.Body)
	}
	if n := s.count(`SELECT count(*) FROM received_events WHERE event_id = $1`, ev.ID); n != 1 {
		t.Fatalf("recorded %d times", n)
	}

	s.needs(pushes...)
	s.configure(nil)
	if r := s.connect(realmA); r.Code != http.StatusOK {
		t.Fatalf("connect %d", r.Code)
	}
	s.c.DeliverEvents()
	s.tick()
	s.tick()
	creates := 0
	for _, c := range s.qb.Writes() {
		if c.Op == "create" && c.Kind == qbo.KindInvoice {
			creates++
		}
	}
	if creates != 1 || s.invoiceRow(inv).State != "synced" {
		t.Fatalf("%d invoice creates", creates)
	}
}

func TestMODQBO20RetryThenFail(t *testing.T) {
	s := newSuite(t)
	s.needs(append(pushes, core03)...)
	s.configure(nil)
	if r := s.connect(realmA); r.Code != http.StatusOK {
		t.Fatalf("connect %d", r.Code)
	}
	inv := s.sentInvoice(s.c.Open, 2, 10000)
	inv2 := s.sentInvoice(s.c.Open, 4, 10000)
	s.c.DeliverEvents()
	s.qb.FailNext(qbo.KindInvoice, unavailable())
	s.tick()
	if r := s.invoiceRow(inv); r.State != "pending" || r.Attempts != 1 || r.NextAttemptAt != s.clock.Now().Add(time.Minute).UnixMicro() {
		t.Fatalf("after one 503 %+v", r)
	}
	waits := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	for i := 0; i < 5; i++ {
		s.qb.FailNext(qbo.KindInvoice, unavailable())
		if i < 4 {
			s.clock.Advance(waits[i])
		}
		s.tick()
	}
	if r := s.invoiceRow(inv); r.State != "synced" {
		t.Fatalf("retry %+v", r)
	}
	if r := s.invoiceRow(inv2); r.State != "failed" || r.Attempts != 5 || r.Code != "503" {
		t.Fatalf("after five 503s %+v", r)
	}
	for _, who := range []string{coretest.Admin, coretest.Accountant} {
		if n := len(s.notifications(who, "quickbooks.sync_problem")); n != 1 {
			t.Fatalf("%s has %d alerts", who, n)
		}
	}
	s.mustAct(coretest.Accountant, "retry", map[string]string{"kind": "invoice", "id": inv2})
	if r := s.invoiceRow(inv2); r.State != "synced" {
		t.Fatalf("after Retry %+v", r)
	}
	if n := s.count(`SELECT count(*) FROM sync_log WHERE invoice_id = $1 AND operation = 'retry_requested' AND actor_user_id = $2`, inv2, s.c.Accountant.ID); n != 1 {
		t.Fatalf("retry log rows %d", n)
	}
}

func TestMODQBO21TimeoutReplay(t *testing.T) {
	s := newConnected(t)
	inv := s.sentInvoice(s.c.Open, 2, 10000)
	s.c.DeliverEvents()
	s.qb.Timeout(qbo.KindInvoice)
	s.tick()
	r := s.invoiceRow(inv)
	if r.State != "pending" || r.RequestID == "" || r.QBOID != "" {
		t.Fatalf("after timeout %+v", r)
	}
	s.clock.Advance(time.Minute)
	s.tick()
	if r2 := s.invoiceRow(inv); r2.State != "synced" || len(s.entities(qbo.KindInvoice)) != 1 {
		t.Fatalf("after retry %+v", r2)
	}
	replayed := false
	for _, c := range s.qb.Calls() {
		replayed = replayed || (c.Kind == qbo.KindInvoice && c.Op == "create" && c.Replayed && c.RequestID == r.RequestID)
	}
	if !replayed {
		t.Fatal("the retry did not reuse the requestid")
	}
}

func TestMODQBO22MappingInvalid(t *testing.T) {
	s := newConnected(t)
	s.setting(map[string]string{"account_trust_liability": "Legal Fee Income"})
	d := s.postTrust(s.c.Open, "deposit", 10000, "Retainer")
	w := s.postTrust(s.c.Open, "withdrawal", 1000, "Costs")
	s.deliver()
	for _, k := range []string{d, w} {
		if r := s.trustRow(k); r.State != "failed" || r.Code != "mapping_invalid" || r.Attempts != 0 {
			t.Fatalf("row %+v", r)
		}
	}
	if n := len(s.entities(qbo.KindJournalEntry)); n != 0 {
		t.Fatalf("%d journal entries posted", n)
	}
	s.setting(map[string]string{"account_trust_liability": "Client Trust Liability"})
	s.tick()
	for _, k := range []string{d, w} {
		if r := s.trustRow(k); r.State != "synced" {
			t.Fatalf("after the fix %+v", r)
		}
	}
}

func TestMODQBO23NoCoreWrites(t *testing.T) {
	s := newSuite(t)
	mod := s.c.Admin.Must(http.StatusOK, "GET", "/api/v1/modules/"+id, nil)
	perms, _ := mod.JSON["permissions"].([]any)
	for _, p := range perms {
		scope := p.(map[string]any)["scope"].(string)
		if !strings.HasSuffix(scope, ":read") && scope != "notifications:write" && scope != "module_secrets:write" {
			t.Fatalf("write scope %s", scope)
		}
	}
	s.timeEntry(s.c.Open, 2, 10000)
	draft := s.c.Attorney.Must(http.StatusCreated, "POST", "/api/v1/invoice-drafts", map[string]any{"matter_id": s.c.Open})
	inv := str(t, draft.JSON["data"].([]any)[0].(map[string]any), "id")
	s.send(inv)
	before := s.invoice(inv)
	payments, ledger := len(s.ids("/api/v1/payments")), len(s.ids("/api/v1/trust-ledger"))
	client := s.c.ModuleClient(id)
	for _, tok := range []string{"", s.ctxFor(coretest.Accountant, "")} {
		for _, call := range []struct {
			method, path string
			body         map[string]any
		}{
			{http.MethodPost, "/api/v1/payments", map[string]any{"matter_id": s.c.Open, "amount_cents": 1, "destination": "operating", "method": "check"}},
			{http.MethodPost, "/api/v1/trust-ledger", map[string]any{"matter_id": s.c.Open, "amount_cents": 1, "entry_type": "deposit"}},
			{http.MethodPatch, "/api/v1/invoices/" + inv, map[string]any{"internal_notes": "changed"}},
			{http.MethodPost, "/api/v1/invoices/" + inv + "/void", map[string]any{"reason": "x"}},
		} {
			var hdr []string
			if tok != "" {
				hdr = []string{coretest.ContextHeader, tok}
			}
			r := client.Do(call.method, call.path, call.body, hdr...)
			if r.Code != http.StatusForbidden && r.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s (context %v): %d %s", call.method, call.path, tok != "", r.Code, r.Body)
			}
		}
	}
	after := s.invoice(inv)
	if cents(after, "total_cents") != cents(before, "total_cents") || after["status"] != before["status"] ||
		len(s.ids("/api/v1/payments")) != payments || len(s.ids("/api/v1/trust-ledger")) != ledger {
		t.Fatalf("core records changed: %v", after)
	}

	// Retry, Re-push and Acknowledge change only the module's rows.
	s.needsRow("invoices", inv)
	s.needs(pushes...)
	s.configure(nil)
	if r := s.connect(realmA); r.Code != http.StatusOK {
		t.Fatalf("connect %d", r.Code)
	}
	s.c.DeliverEvents()
	s.tick()
	s.qb.EditAsAccountant(realmA, qbo.KindInvoice, s.invoiceRow(inv).QBOID)
	s.syncNow()
	s.mustAct(coretest.Accountant, "repush", map[string]string{"kind": "invoice", "id": inv})
	s.qb.EditAsAccountant(realmA, qbo.KindInvoice, s.invoiceRow(inv).QBOID)
	s.clock.Advance(time.Minute)
	s.syncNow()
	s.mustAct(coretest.Accountant, "acknowledge", map[string]string{"kind": "invoice", "id": inv})
	if a := s.invoice(inv); cents(a, "total_cents") != cents(before, "total_cents") || a["status"] != before["status"] {
		t.Fatalf("core invoice changed: %v", a)
	}
}

func TestMODQBO24Roles(t *testing.T) {
	s := newSuite(t)
	body := map[string]string{"kind": "invoice", "id": "no-such-invoice", "confirm": "RESET", "client_secret": "x"}
	for _, who := range []string{coretest.Staff, coretest.Attorney} {
		tok := s.ctxFor(who, "")
		for _, a := range []string{"retry", "repush", "acknowledge", "sync-now", "reset", "disconnect", "create-missing", "client-secret", "trust-bank-map"} {
			if r := s.act(who, a, tok, body); r.Code != http.StatusForbidden {
				t.Fatalf("%s as %s: %d %s", a, who, r.Code, r.Body)
			}
		}
		if r := s.get(who, "/api/setup", tok); r.Code != http.StatusNotFound {
			t.Fatalf("setup as %s: %d", who, r.Code)
		}
	}
	acct := s.ctxFor(coretest.Accountant, "")
	for _, a := range []string{"disconnect", "create-missing", "reset", "client-secret", "trust-bank-map"} {
		if r := s.act(coretest.Accountant, a, acct, body); r.Code != http.StatusForbidden {
			t.Fatalf("%s as accountant: %d", a, r.Code)
		}
	}
	s.mustAct(coretest.Admin, "reset", map[string]string{"confirm": "RESET"})

	s.needs(pushes...)
	s.configure(nil)
	if r := s.connect(realmA); r.Code != http.StatusOK {
		t.Fatalf("connect %d", r.Code)
	}
	if v := decode[quickbooks.SetupView](t, s.get(coretest.Accountant, "/api/setup", acct)); v.ConnectURL != "" || v.CanAct {
		t.Fatalf("accountant setup %+v", v)
	}
	s.mustAct(coretest.Admin, "create-missing", nil)
	s.mustAct(coretest.Accountant, "sync-now", nil)
	s.mustAct(coretest.Admin, "disconnect", nil)
}

// nextReconcile is the next 08:00 UTC after t, the default reconcile hour.
func nextReconcile(t time.Time) time.Time {
	n := time.Date(t.Year(), t.Month(), t.Day(), 8, 0, 0, 0, time.UTC)
	if !n.After(t) {
		n = n.AddDate(0, 0, 1)
	}
	return n
}

func TestMODQBO25NightlyFindsMissedEvents(t *testing.T) {
	s := newConnected(t)
	inv := s.sentInvoice(s.c.Open, 2, 10000)
	dep := s.postTrust(s.c.Open, "deposit", 5000, "Retainer")
	// The events are never delivered.
	s.tick()
	if _, ok := s.row("invoice_sync", "invoice_id", inv); ok {
		t.Fatal("a row exists with no event")
	}
	s.clock.Set(nextReconcile(s.clock.Now()))
	s.tick()
	if s.invoiceRow(inv).State != "synced" || s.trustRow(dep).State != "synced" {
		t.Fatal("the nightly run did not push the missed records")
	}
	before := len(s.qb.Writes())
	s.syncNow()
	if after := len(s.qb.Writes()); after != before {
		t.Fatalf("a second run with no changes made %d writes", after-before)
	}
}

func TestMODQBO26Drift(t *testing.T) {
	s := newConnected(t)
	inv := s.sentInvoice(s.c.Open, 20, 5000)
	s.deliver()
	qid := s.invoiceRow(inv).QBOID
	pushed := s.qb.Get(realmA, qbo.KindInvoice, qid).(*qbo.Invoice)
	s.qb.EditAsAccountant(realmA, qbo.KindInvoice, qid)
	s.syncNow()
	if r := s.invoiceRow(inv); r.State != "drift" {
		t.Fatalf("after the accountant edit %+v", r)
	}
	books := decode[quickbooks.BooksView](t, s.get(coretest.Accountant, "/api/books?status=exceptions", s.ctxFor(coretest.Accountant, "")))
	if len(books.Rows) != 1 || !books.Rows[0].CanDrift {
		t.Fatalf("exceptions %+v", books.Rows)
	}
	s.mustAct(coretest.Accountant, "repush", map[string]string{"kind": "invoice", "id": inv})
	got := s.qb.Get(realmA, qbo.KindInvoice, qid).(*qbo.Invoice)
	if s.invoiceRow(inv).State != "synced" || got.Line[0].Description != pushed.Line[0].Description || *got.TotalAmt != *pushed.TotalAmt {
		t.Fatalf("re-push %+v", got)
	}
	s.qb.EditAsAccountant(realmA, qbo.KindInvoice, qid)
	s.syncNow()
	s.mustAct(coretest.Accountant, "acknowledge", map[string]string{"kind": "invoice", "id": inv})
	s.syncNow()
	if r := s.invoiceRow(inv); r.State != "acknowledged" {
		t.Fatalf("acknowledged row came back %+v", r)
	}
	s.writeOff(inv, 1000) // the Togavsu invoice changes
	s.deliver()
	if r := s.invoiceRow(inv); r.State != "drift" {
		t.Fatalf("after a Togavsu change %+v", r)
	}
}

func TestMODQBO27Customers(t *testing.T) {
	s := newConnected(t)
	s.postTrust(s.c.Open, "deposit", 1000, "Retainer")
	existingMatter, existing := s.matter("Matter Existing")
	adopted := s.qb.AddCustomer(realmA, qbo.Customer{DisplayName: "Client Existing"})
	s.postTrust(existingMatter, "deposit", 1000, "Retainer")
	s.qb.AddVendor(realmA, "Client Vendor")
	vendorMatter, vendor := s.matter("Matter Vendor")
	s.postTrust(vendorMatter, "deposit", 1000, "Retainer")
	s.deliver()
	name := str(t, s.c.Admin.Must(http.StatusOK, "GET", "/api/v1/contacts/"+s.c.ClientContact, nil).JSON, "display_name")
	first := s.qb.Get(realmA, qbo.KindCustomer, s.customerID(s.c.ClientContact)).(*qbo.Customer)
	if first.DisplayName != name || !strings.Contains(first.Notes, "Togavsu contact "+s.c.ClientContact) {
		t.Fatalf("customer %+v", first)
	}
	if s.customerID(existing) != adopted || s.count(`SELECT count(*) FROM sync_log WHERE contact_id = $1 AND operation = 'adopt'`, existing) != 1 {
		t.Fatal("the existing customer was not adopted")
	}
	v := s.qb.Get(realmA, qbo.KindCustomer, s.customerID(vendor)).(*qbo.Customer)
	if v.DisplayName != "Client Vendor ("+strings.ReplaceAll(vendor, "-", "")[:8]+")" {
		t.Fatalf("vendor name %q", v.DisplayName)
	}
	s.c.Admin.Must(http.StatusOK, "PATCH", "/api/v1/contacts/"+s.c.ClientContact, map[string]any{"family_name": "Harlow-Example"})
	s.deliver()
	if c := s.qb.Get(realmA, qbo.KindCustomer, s.customerID(s.c.ClientContact)).(*qbo.Customer); !strings.Contains(c.DisplayName, "Harlow-Example") {
		t.Fatalf("after contact.updated %+v", c)
	}
}

// qboMentions reports whether any QBO entity names one of needles.
func qboMentions(s *suite, needles ...string) bool {
	for _, k := range []qbo.Kind{qbo.KindCustomer, qbo.KindInvoice, qbo.KindPayment, qbo.KindJournalEntry, qbo.KindCreditMemo} {
		for _, ent := range s.entities(k) {
			raw, _ := json.Marshal(ent)
			for _, n := range needles {
				if strings.Contains(string(raw), n) {
					return true
				}
			}
		}
	}
	return false
}

func TestMODQBO28WalledMatter(t *testing.T) {
	s := newConnected(t)
	m43, wally := s.matter("Estate of Wallace")
	s.c.Wall(m43, s.c.Attorney)
	inv43 := s.sentInvoice(m43, 10, 2500)
	pay43 := s.payment(m43, inv43, "operating", "check", 10000)
	dep43 := s.postTrust(m43, "deposit", 70000, "Walled retainer, whole description")
	ids43 := []string{inv43, pay43, dep43, wally, m43}
	inv42 := s.sentInvoice(s.c.Open, 10, 3000)
	s.deliver()

	// No event for the walled records reaches the module.
	for _, k := range ids43[:3] {
		if n := s.count(`SELECT count(*) FROM received_events WHERE record_id = $1`, k); n != 0 {
			t.Fatalf("an event for walled record %s reached /events", k)
		}
	}
	// The books reader leaves them out.
	mod := s.c.ModuleClient(id)
	for _, p := range []string{"/api/v1/invoices/" + inv43, "/api/v1/payments/" + pay43, "/api/v1/trust-ledger/" + dep43,
		"/api/v1/books/matters/" + m43, "/api/v1/books/contacts/" + wally} {
		if r := mod.Do("GET", p, nil); r.Code != http.StatusNotFound {
			t.Fatalf("books reader %s: %d", p, r.Code)
		}
	}
	if r := mod.Do("GET", "/api/v1/trust/balances", nil); strings.Contains(r.Body, "70000") {
		t.Fatalf("balances include the walled deposit: %s", r.Body)
	}
	// A nightly reconcile still sees nothing of them.
	s.qb.ResetCalls()
	s.clock.Set(nextReconcile(s.clock.Now()))
	s.tick()
	dump := s.dump()
	for _, k := range ids43 {
		if strings.Contains(dump, k) {
			t.Fatalf("mod_quickbooks holds a row for %s", k)
		}
	}
	if qboMentions(s, "Wallace", inv43, dep43, pay43) {
		t.Fatal("QBO holds an entity for the walled matter")
	}
	for _, c := range s.qb.Calls() {
		if c.IsWrite() {
			t.Fatalf("the reconcile wrote %+v", c)
		}
	}
	screens := func(want int) {
		t.Helper()
		for _, who := range []string{coretest.Admin, coretest.Accountant} {
			w := decode[quickbooks.WidgetView](t, s.get(who, "/api/status", s.ctxFor(who, "")))
			if total := w.Counts.Synced + w.Counts.Pending + w.Counts.Exceptions; total != want {
				t.Fatalf("widget counts %+v, want %d", w.Counts, want)
			}
			b := s.get(who, "/api/books?status=synced", s.ctxFor(who, ""))
			for _, k := range ids43 {
				if strings.Contains(b.Body, k) {
					t.Fatal("the books tab shows a walled record")
				}
			}
		}
		for _, who := range []string{coretest.Admin, coretest.Attorney} {
			r := s.get(who, "/api/matter", s.ctxFor(who, m43))
			for _, k := range ids43[:3] {
				if strings.Contains(r.Body, k) {
					t.Fatalf("the matter tab for %s shows a walled record", who)
				}
			}
		}
	}
	screens(2) // the open matter's invoice and its customer

	// Walling the open matter hides its synced invoice at the next reconcile.
	s.c.Wall(s.c.Open, s.c.Attorney)
	s.syncNow()
	if r := s.invoiceRow(inv42); r.State != "hidden" {
		t.Fatalf("open matter invoice %+v", r)
	}
	screens(0)

	// Lifting both walls and pressing Sync now pushes everything.
	for _, m := range []string{m43, s.c.Open} {
		s.c.Admin.Must(http.StatusOK, "PUT", "/api/v1/matters/"+m+"/wall", map[string]any{"user_ids": []string{}})
	}
	s.syncNow()
	for _, r := range []rowState{s.invoiceRow(inv43), s.paymentRow(pay43), s.trustRow(dep43), s.invoiceRow(inv42)} {
		if r.State != "synced" {
			t.Fatalf("after lifting the walls %+v", r)
		}
	}
	je := journal(t, s.qb.Get(realmA, qbo.KindJournalEntry, s.trustRow(dep43).QBOID))
	if je.Line[0].Description != "Walled retainer, whole description" {
		t.Fatalf("description %q", je.Line[0].Description)
	}
	if c := s.qb.Get(realmA, qbo.KindCustomer, s.customerID(wally)).(*qbo.Customer); c.DisplayName != "Client Wallace" {
		t.Fatalf("customer %+v", c)
	}
}
