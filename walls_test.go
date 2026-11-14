package quickbooks_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	quickbooks "github.com/CharitonMedia/togavsu-quickbooks"
	"github.com/CharitonMedia/togavsu-quickbooks/internal/fakecore"
	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
)

// mentions reports whether any QBO call or entity names one of the ids or the text.
func qboMentions(e *env, needles ...string) bool {
	for _, k := range []qbo.Kind{qbo.KindCustomer, qbo.KindInvoice, qbo.KindPayment, qbo.KindJournalEntry, qbo.KindCreditMemo} {
		for _, ent := range e.entities(k) {
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
	e := newEnv(t)
	wally := e.core.AddContact(quickbooks.BooksContact{GivenName: "Wally", FamilyName: "Walled"})
	m43 := e.core.AddMatter("2026-0043", "Walled matter", wally)
	e.core.Wall(m43, e.attorney)
	inv43 := e.sentInvoice(m43, "INV-4300", 25000)
	pay43 := e.core.RecordPayment(fakecore.PaymentInput{MatterID: m43, InvoiceID: inv43, AmountCents: 10000})
	dep43 := e.core.PostTrust(m43, e.trust, "deposit", 70000, "Walled retainer, whole description")
	ids43 := []string{inv43, pay43, dep43, wally, m43}

	inv42 := e.sentInvoice(e.matter, "INV-4200", 30000)
	e.tick()

	// no event for the walled records reaches the module
	for _, ev := range e.core.Delivered() {
		for _, id := range ids43 {
			if ev.RecordID == id || ev.MatterID == id {
				t.Fatalf("event %s for a walled record reached /events", ev.Action)
			}
		}
	}
	// the books reader leaves them out
	for _, p := range []string{"/api/v1/invoices/" + inv43, "/api/v1/payments/" + pay43, "/api/v1/trust-ledger/" + dep43,
		"/api/v1/books/matters/" + m43, "/api/v1/books/contacts/" + wally} {
		var v map[string]any
		if err := coreGet(t, e, p, "", &v); err == nil || !strings.Contains(err.Error(), "404") {
			t.Fatalf("books reader %s: %v", p, err)
		}
	}
	var bal struct {
		Data []quickbooks.TrustBalanceRow `json:"data"`
	}
	if err := coreGet(t, e, "/api/v1/trust/balances", "", &bal); err != nil || len(bal.Data) != 0 {
		t.Fatalf("balances include the walled deposit: %+v %v", bal, err)
	}

	// a nightly reconcile still sees nothing of them
	e.qb.ResetCalls()
	e.clock.Advance(17 * time.Hour)
	e.tick()
	dump := dumpModule(t, e)
	for _, id := range ids43 {
		if strings.Contains(dump, id) {
			t.Fatalf("mod_quickbooks holds a row for %s", id)
		}
	}
	if qboMentions(e, "Wally", inv43, dep43, pay43) {
		t.Fatal("QBO holds an entity for the walled matter")
	}
	for _, c := range e.qb.Calls() {
		if c.IsWrite() {
			t.Fatalf("the reconcile wrote %+v", c)
		}
	}

	// no screen shows them, to the admin, the accountant or the attorney on the wall
	screens := func(want int) {
		t.Helper()
		for _, u := range []string{e.admin, e.accountant} {
			w := decode[quickbooks.WidgetView](t, e.get("/api/status", e.ctxFor(u, "")))
			if total := w.Counts.Synced + w.Counts.Pending + w.Counts.Exceptions; total != want {
				t.Fatalf("widget counts %+v, want %d", w.Counts, want)
			}
			b := e.get("/api/books?status=synced&month=2026-03", e.ctxFor(u, ""))
			for _, id := range ids43 {
				if strings.Contains(b.body, id) {
					t.Fatal("the books tab shows a walled record")
				}
			}
		}
		for _, u := range []string{e.admin, e.attorney} {
			r := e.get("/api/matter", e.ctxFor(u, m43))
			for _, id := range ids43[:3] {
				if strings.Contains(r.body, id) {
					t.Fatalf("matter tab for %s shows a walled record", u)
				}
			}
		}
	}
	screens(1)
	for _, n := range e.core.Notifications {
		if strings.Contains(n.Title, "Wally") {
			t.Fatal("an alert names the walled matter")
		}
	}

	// walling 2026-0042 hides its synced invoice; a change under the wall makes no call
	e.core.Wall(e.matter, e.attorney)
	e.syncNow()
	if r := e.invoiceRow(inv42); r.State != "hidden" {
		t.Fatalf("2026-0042 invoice %+v", r)
	}
	screens(0)
	e.core.EditInvoice(inv42, func(i *quickbooks.Invoice) {
		i.Lines = append(i.Lines, quickbooks.InvoiceLine{Kind: "time", Description: "Under the wall", AmountCents: 700})
		i.TotalCents += 700
	})
	e.qb.ResetCalls()
	e.tick()
	e.clock.Advance(time.Minute)
	e.syncNow()
	for _, c := range e.qb.Calls() {
		if c.IsWrite() {
			t.Fatalf("a write while walled: %+v", c)
		}
	}

	// lifting both walls and pressing Sync now pushes everything
	e.core.Unwall(m43)
	e.core.Unwall(e.matter)
	e.syncNow()
	for _, r := range []rowState{e.invoiceRow(inv43), e.paymentRow(pay43), e.trustRow(dep43), e.invoiceRow(inv42)} {
		if r.State != "synced" {
			t.Fatalf("after lifting the walls %+v", r)
		}
	}
	je := journal(t, e.qb.Get(realmA, qbo.KindJournalEntry, e.trustRow(dep43).QBOID))
	if je.Line[0].Description != "Walled retainer, whole description" {
		t.Fatalf("description %q", je.Line[0].Description)
	}
	if c := e.qb.Get(realmA, qbo.KindCustomer, e.customerID(wally)).(*qbo.Customer); c.DisplayName != "Wally Walled" {
		t.Fatalf("customer %+v", c)
	}
	got := e.qb.Get(realmA, qbo.KindInvoice, e.invoiceRow(inv42).QBOID).(*qbo.Invoice)
	if int64(*got.TotalAmt) != 30700 {
		t.Fatalf("2026-0042 invoice total %v", got.TotalAmt)
	}
	if r := e.get("/api/books", e.ctxFor(e.admin, "")); r.status != http.StatusOK {
		t.Fatal(r.body)
	}
}
