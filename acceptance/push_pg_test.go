package acceptance_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
)

// Account and item ids in fake.SampleChart: 4 Undeposited Funds, 35 Operating Checking,
// 36 IOLTA Trust, 79 and 80 income, 90 Client Trust Liability; items 1 Legal Services,
// 3 Reimbursed Costs, 4 Invoice Adjustment.

func TestMODQBO06InvoiceLines(t *testing.T) {
	s := newConnected(t)
	s.timeEntry(s.c.Open, 8, 30000)
	s.expense(s.c.Open, "Filing fee", 4500, true)
	inv := s.draft(s.c.Open)
	s.c.Attorney.Must(http.StatusOK, "PATCH", "/api/v1/invoices/"+inv, map[string]any{"adjustment_cents": -1000, "adjustment_reason": "rounding"})
	s.send(inv)
	s.deliver()
	r := s.invoiceRow(inv)
	if r.State != "synced" {
		t.Fatalf("row %+v", r)
	}
	core := s.invoice(inv)
	got := s.qb.Get(realmA, qbo.KindInvoice, r.QBOID).(*qbo.Invoice)
	if got.DocNumber != core["number"] || got.EmailStatus != "NotSet" {
		t.Fatalf("header %+v", got)
	}
	items := map[string]string{"time": "1", "expense": "3", "adjustment": "4"}
	lines, _ := core["lines"].([]any)
	for i, l := range lines {
		line := l.(map[string]any)
		q := got.Line[i]
		if q.SalesItemLineDetail.ItemRef.Value != items[line["kind"].(string)] || int64(q.Amount) != cents(line, "amount_cents") {
			t.Fatalf("line %d: %+v against %v", i, q, line)
		}
	}
	if got.TotalAmt == nil || int64(*got.TotalAmt) != cents(core, "total_cents") {
		t.Fatalf("total %v, Togavsu %d", got.TotalAmt, cents(core, "total_cents"))
	}
}

func TestMODQBO07DraftAndBeforeStart(t *testing.T) {
	s := newConnected(t)
	s.timeEntry(s.c.Open, 2, 10000)
	draft := s.draft(s.c.Open)
	s.deliver()
	if r := s.invoiceRow(draft); r.State != "skipped" || r.QBOID != "" {
		t.Fatalf("draft %+v", r)
	}
	tomorrow := s.clock.Now().AddDate(0, 0, 1).Format(time.DateOnly)
	s.setting(map[string]string{"sync_start_date": tomorrow})
	old := s.sentInvoice(s.c.Open, 2, 10000)
	s.deliver()
	if r := s.invoiceRow(old); r.State != "skipped" || !strings.Contains(r.SkipReason, "start date "+tomorrow) {
		t.Fatalf("before the start date %+v", r)
	}
	if n := len(s.entities(qbo.KindInvoice)); n != 0 {
		t.Fatalf("%d invoices reached QBO", n)
	}
}

func TestMODQBO08TrustDepositNotIncome(t *testing.T) {
	s := newConnected(t)
	before := s.qb.Balances(realmA)
	dep := s.postTrust(s.c.Open, "deposit", 150000, "Retainer deposit")
	s.deliver()
	r := s.trustRow(dep)
	if r.State != "synced" {
		t.Fatalf("row %+v", r)
	}
	cust := s.customerID(s.c.ClientContact)
	je := journal(t, s.qb.Get(realmA, qbo.KindJournalEntry, r.QBOID))
	if got, want := lineSet(je), []string{"Debit 36 150000", "Credit 90 150000 customer " + cust}; !equal(got, want) {
		t.Fatalf("lines %v, want %v", got, want)
	}
	after := s.qb.Balances(realmA)
	for _, acct := range []string{"79", "80"} {
		if after[acct] != before[acct] {
			t.Fatalf("income account %s moved", acct)
		}
	}
	name := str(t, s.c.Admin.Must(http.StatusOK, "GET", "/api/v1/contacts/"+s.c.ClientContact, nil).JSON, "display_name")
	if cu := s.qb.Get(realmA, qbo.KindCustomer, cust).(*qbo.Customer); cu.DisplayName != name {
		t.Fatalf("customer %+v, contact %q", cu, name)
	}
}

func TestMODQBO09TrustPaymentSkipped(t *testing.T) {
	s := newConnected(t)
	p := s.payment(s.c.Open, "", "trust", "check", 80000)
	s.deliver()
	if r := s.paymentRow(p); r.State != "skipped" || r.SkipReason != "posted from trust ledger entry" {
		t.Fatalf("payment %+v", r)
	}
	if n := len(s.entities(qbo.KindPayment)); n != 0 {
		t.Fatalf("%d payments in QBO", n)
	}
	if b := s.qb.Balances(realmA)["36"]; b != 80000 {
		t.Fatalf("trust bank %d", b)
	}
}

// applied checks the pair an apply makes: the two-line trust entry and the payment into
// operating applied to the invoice.
func applied(t *testing.T, s *suite, client, inv, entry, pay string, amount int64) {
	t.Helper()
	cust := s.customerID(client)
	je := journal(t, s.qb.Get(realmA, qbo.KindJournalEntry, s.trustRow(entry).QBOID))
	if got, want := lineSet(je), []string{"Debit 90 " + itoa(amount) + " customer " + cust, "Credit 36 " + itoa(amount)}; !equal(got, want) {
		t.Fatalf("lines %v, want %v", got, want)
	}
	p := s.qb.Get(realmA, qbo.KindPayment, s.paymentRow(pay).QBOID).(*qbo.Payment)
	if int64(p.TotalAmt) != amount || p.DepositToAccountRef.Value != "35" || len(p.Line) != 1 || p.Line[0].LinkedTxn[0].TxnID != s.invoiceRow(inv).QBOID {
		t.Fatalf("payment %+v", p)
	}
}

func TestMODQBO10ApplyTrust(t *testing.T) {
	s := newConnected(t)
	inv := s.sentInvoice(s.c.Open, 8, 25000)
	s.postTrust(s.c.Open, "deposit", 100000, "Retainer")
	s.deliver()
	before := s.qb.InvoiceBalance(realmA, s.invoiceRow(inv).QBOID)
	entry, pay := s.applyTrust(inv, 50000)
	s.deliver()
	applied(t, s, s.c.ClientContact, inv, entry, pay, 50000)
	after := s.qb.InvoiceBalance(realmA, s.invoiceRow(inv).QBOID)
	if int64(before-after) != 50000 || int64(after) != cents(s.invoice(inv), "balance_due_cents") {
		t.Fatalf("balance %v to %v", before, after)
	}
}

func TestMODQBO11FlatFeeApply(t *testing.T) {
	s := newConnected(t)
	matter, _ := s.matter("Estate of Flatley")
	s.c.Admin.Must(http.StatusOK, "PATCH", "/api/v1/matters/"+matter, map[string]any{"billing_type": "flat_fee", "flat_fee_cents": 150000})
	s.postTrust(matter, "deposit", 150000, "Flat fee deposit")
	entry, pay := s.applyFlatFee(matter, 150000)
	s.deliver()
	inv := str(t, s.c.Admin.Must(http.StatusOK, "GET", "/api/v1/payments/"+pay, nil).JSON, "invoice_id")
	applied(t, s, s.clientOf(matter), inv, entry, pay, 150000)
	if b := s.qb.InvoiceBalance(realmA, s.invoiceRow(inv).QBOID); b != 0 {
		t.Fatalf("balance %v", b)
	}
}

func TestMODQBO12TransferNoInvoice(t *testing.T) {
	s := newConnected(t)
	s.postTrust(s.c.Open, "deposit", 50000, "Retainer")
	tr := s.postTrust(s.c.Open, "transfer_to_operating", 20000, "Earned fees")
	s.deliver()
	cust := s.customerID(s.c.ClientContact)
	je := journal(t, s.qb.Get(realmA, qbo.KindJournalEntry, s.trustRow(tr).QBOID))
	want := []string{"Debit 90 20000 customer " + cust, "Credit 36 20000", "Debit 35 20000", "Credit 79 20000"}
	if got := lineSet(je); !equal(got, want) {
		t.Fatalf("lines %v, want %v", got, want)
	}
}

func TestMODQBO13TrustShapes(t *testing.T) {
	s := newConnected(t)
	dep := s.postTrust(s.c.Open, "deposit", 90000, "Retainer")
	wd := s.postTrust(s.c.Open, "withdrawal", 10000, "Court costs")
	rf := s.postTrust(s.c.Open, "refund", 5000, "Refund to client")
	up := s.postTrust(s.c.Open, "adjustment", 300, "Bank correction up")
	down := s.postTrust(s.c.Open, "adjustment", -200, "Bank correction down")
	rev := s.reverseTrust(wd)
	s.deliver()
	c := " customer " + s.customerID(s.c.ClientContact)
	cases := map[string][]string{
		dep:  {"Debit 36 90000", "Credit 90 90000" + c},
		wd:   {"Debit 90 10000" + c, "Credit 36 10000"},
		rf:   {"Debit 90 5000" + c, "Credit 36 5000"},
		up:   {"Debit 36 300", "Credit 90 300" + c},
		down: {"Debit 90 200" + c, "Credit 36 200"},
		rev:  {"Credit 90 10000" + c, "Debit 36 10000"},
	}
	for k, want := range cases {
		je := journal(t, s.qb.Get(realmA, qbo.KindJournalEntry, s.trustRow(k).QBOID))
		if got := lineSet(je); !equal(got, want) {
			t.Fatalf("%s: lines %v, want %v", k, got, want)
		}
	}
	for _, ent := range s.entities(qbo.KindJournalEntry) {
		for _, l := range journal(t, ent).Line {
			if a := l.JournalEntryLineDetail.AccountRef.Value; a == "79" || a == "80" {
				t.Fatalf("income line in %+v", ent)
			}
		}
	}
}

func TestMODQBO14BelowZeroWithdrawal(t *testing.T) {
	s := newConnected(t)
	s.postTrust(s.c.Open, "deposit", 1000, "Small deposit")
	wd := s.postTrust(s.c.Open, "withdrawal", 5000, "Overdrawn withdrawal")
	s.deliver()
	if r := s.trustRow(wd); r.State != "synced" {
		t.Fatalf("row %+v", r)
	}
	if b := s.qb.Balances(realmA)["36"]; b != -4000 {
		t.Fatalf("trust bank %d", b)
	}
}

func TestMODQBO15OperatingPayment(t *testing.T) {
	s := newConnected(t)
	inv := s.sentInvoice(s.c.Open, 8, 5000)
	s.deliver()
	p1 := s.payment(s.c.Open, inv, "operating", "check", 15000)
	s.deliver()
	pm := s.qb.Get(realmA, qbo.KindPayment, s.paymentRow(p1).QBOID).(*qbo.Payment)
	if pm.DepositToAccountRef.Value != "4" || pm.Line[0].LinkedTxn[0].TxnID != s.invoiceRow(inv).QBOID {
		t.Fatalf("payment %+v", pm)
	}
	// A payment on an invoice not yet synced waits, then posts after it.
	s.setting(map[string]string{"payment_deposit": "operating_bank"})
	inv2 := s.sentInvoice(s.c.Open, 4, 6250)
	p2 := s.payment(s.c.Open, inv2, "operating", "ach", 25000)
	s.c.DeliverEvents()
	s.qb.FailNext(qbo.KindInvoice, unavailable())
	s.tick()
	if r := s.paymentRow(p2); r.State != "pending" || r.WaitingOn != "invoice" || r.Attempts != 0 {
		t.Fatalf("waiting payment %+v", r)
	}
	s.clock.Advance(time.Minute)
	s.tick()
	r := s.paymentRow(p2)
	pm2 := s.qb.Get(realmA, qbo.KindPayment, r.QBOID).(*qbo.Payment)
	if r.State != "synced" || pm2.DepositToAccountRef.Value != "35" || pm2.Line[0].LinkedTxn[0].TxnID != s.invoiceRow(inv2).QBOID {
		t.Fatalf("payment %+v %+v", r, pm2)
	}
}

func TestMODQBO16ReversedPayment(t *testing.T) {
	s := newConnected(t)
	inv := s.sentInvoice(s.c.Open, 6, 5000)
	p := s.payment(s.c.Open, inv, "operating", "check", 30000)
	s.deliver()
	s.reversePayment(p)
	s.deliver()
	if !s.qb.Voided(realmA, qbo.KindPayment, s.paymentRow(p).QBOID) {
		t.Fatal("payment not voided in QBO")
	}
	never := s.payment(s.c.Open, "", "operating", "check", 5000)
	s.reversePayment(never)
	s.qb.ResetCalls()
	s.deliver()
	if r := s.paymentRow(never); r.State != "skipped" || r.QBOID != "" {
		t.Fatalf("never synced %+v", r)
	}
	for _, c := range s.qb.Writes() {
		if c.Op == "void" {
			t.Fatalf("unexpected void %+v", c)
		}
	}
}

func TestMODQBO17VoidedInvoice(t *testing.T) {
	s := newConnected(t)
	inv := s.sentInvoice(s.c.Open, 12, 5000)
	p := s.payment(s.c.Open, inv, "operating", "check", 20000)
	s.writeOff(inv, 5000)
	s.deliver()
	ir := s.invoiceRow(inv)
	s.reversePayment(p)
	s.void(inv)
	s.qb.ResetCalls()
	s.deliver()
	if !s.qb.Voided(realmA, qbo.KindInvoice, ir.QBOID) || !s.qb.Voided(realmA, qbo.KindPayment, s.paymentRow(p).QBOID) {
		t.Fatal("not voided")
	}
	var order []string
	for _, c := range s.qb.Writes() {
		order = append(order, c.Op+" "+string(c.Kind))
	}
	if want := []string{"void Payment", "delete Payment", "delete CreditMemo", "void Invoice"}; !equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	// A draft voided before sync makes no QBO call.
	s.timeEntry(s.c.Open, 1, 10000)
	d := s.draft(s.c.Open)
	s.void(d)
	s.qb.ResetCalls()
	s.deliver()
	if n := len(s.qb.Writes()); n != 0 {
		t.Fatalf("%d writes", n)
	}
	if r := s.invoiceRow(d); r.State != "skipped" {
		t.Fatalf("draft %+v", r)
	}
}

func TestMODQBO18WriteOff(t *testing.T) {
	s := newConnected(t)
	inv := s.sentInvoice(s.c.Open, 20, 5000)
	s.deliver()
	s.writeOff(inv, 12500)
	s.deliver()
	r := s.invoiceRow(inv)
	if cms := s.entities(qbo.KindCreditMemo); len(cms) != 1 {
		t.Fatalf("%d credit memos", len(cms))
	}
	if b := s.qb.InvoiceBalance(realmA, r.QBOID); int64(b) != cents(s.invoice(inv), "balance_due_cents") {
		t.Fatalf("balance %v", b)
	}
	s.writeOff(inv, 7500) // the write-off on the invoice is now 20000
	s.deliver()
	cms := s.entities(qbo.KindCreditMemo)
	if len(cms) != 1 || int64(cms[0].(*qbo.CreditMemo).Line[0].Amount) != 20000 {
		t.Fatalf("credit memos %+v", cms)
	}
	if b := s.qb.InvoiceBalance(realmA, r.QBOID); int64(b) != cents(s.invoice(inv), "balance_due_cents") {
		t.Fatalf("balance %v", b)
	}
	zero := 0
	for _, ent := range s.entities(qbo.KindPayment) {
		if ent.(*qbo.Payment).TotalAmt == 0 {
			zero++
		}
	}
	if zero != 1 {
		t.Fatalf("%d zero payments", zero)
	}
}
