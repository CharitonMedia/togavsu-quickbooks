package quickbooks_test

import (
	"strings"
	"testing"
	"time"

	quickbooks "github.com/CharitonMedia/togavsu-quickbooks"
	"github.com/CharitonMedia/togavsu-quickbooks/internal/fakecore"
	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
)

func TestMODQBO06InvoiceLines(t *testing.T) {
	e := newEnv(t)
	id := e.core.CreateInvoice(fakecore.InvoiceInput{MatterID: e.matter, Number: "INV-1001", IssuedOn: "2026-03-01",
		Lines: []quickbooks.InvoiceLine{
			{Kind: "time", Description: "Research, 2.0 h", AmountCents: 60000},
			{Kind: "expense", Description: "Filing fee", AmountCents: 4500},
			{Kind: "flat_fee", Description: "Demand letter", AmountCents: 50000},
			{Kind: "adjustment", Description: "Courtesy discount", AmountCents: -2500},
		},
		AdjustmentCents: -1000, AdjustmentNote: "rounding", InterestCents: 735})
	e.core.SendInvoice(id)
	e.tick()
	r := e.invoiceRow(id)
	if r.State != "synced" {
		t.Fatalf("row %+v", r)
	}
	inv := e.qb.Get(realmA, qbo.KindInvoice, r.QBOID).(*qbo.Invoice)
	if inv.DocNumber != "INV-1001" || inv.EmailStatus != "NotSet" || inv.AllowOnlinePayment {
		t.Fatalf("header %+v", inv)
	}
	want := []struct {
		item  string
		cents int64
	}{{"1", 60000}, {"3", 4500}, {"2", 50000}, {"4", -2500}, {"4", -1000}, {"5", 735}}
	if len(inv.Line) != len(want) {
		t.Fatalf("lines %+v", inv.Line)
	}
	for i, w := range want {
		l := inv.Line[i]
		if l.SalesItemLineDetail.ItemRef.Value != w.item || int64(l.Amount) != w.cents {
			t.Fatalf("line %d: %+v", i, l)
		}
	}
	if inv.TotalAmt == nil || int64(*inv.TotalAmt) != e.core.Invoice(id).TotalCents || int64(*inv.TotalAmt) != 111735 {
		t.Fatalf("total %v", inv.TotalAmt)
	}
	if !strings.Contains(inv.PrivateNote, "Togavsu invoice "+id) || !strings.Contains(inv.PrivateNote, "2026-0042") {
		t.Fatalf("note %q", inv.PrivateNote)
	}
}

func TestMODQBO08TrustDepositNotIncome(t *testing.T) {
	e := newEnv(t)
	before := e.qb.Balances(realmA)
	id := e.core.PostTrust(e.matter, e.trust, "deposit", 150000, "Retainer deposit")
	e.tick()
	r := e.trustRow(id)
	if r.State != "synced" {
		t.Fatalf("row %+v", r)
	}
	je := journal(t, e.qb.Get(realmA, qbo.KindJournalEntry, r.QBOID))
	cust := e.customerID(e.jane)
	want := []string{"Debit 36 150000", "Credit 90 150000 customer " + cust}
	if got := lineSet(je); !equal(got, want) {
		t.Fatalf("lines %v, want %v", got, want)
	}
	after := e.qb.Balances(realmA)
	for _, acct := range []string{"79", "80"} {
		if after[acct] != before[acct] {
			t.Fatalf("income account %s moved: %d to %d", acct, before[acct], after[acct])
		}
	}
	if after["36"] != 150000 {
		t.Fatalf("trust bank %d", after["36"])
	}
	cu := e.qb.Get(realmA, qbo.KindCustomer, cust).(*qbo.Customer)
	if cu.DisplayName != "Jane Sample" {
		t.Fatalf("customer %+v", cu)
	}
}

func TestMODQBO07DraftAndBeforeStart(t *testing.T) {
	e := newEnv(t)
	draft := e.core.CreateInvoice(fakecore.InvoiceInput{MatterID: e.matter, Number: "INV-D", IssuedOn: "2026-03-01",
		Lines: []quickbooks.InvoiceLine{{Kind: "time", AmountCents: 1000}}})
	old := e.core.CreateInvoice(fakecore.InvoiceInput{MatterID: e.matter, Number: "INV-OLD", IssuedOn: "2025-12-15",
		Lines: []quickbooks.InvoiceLine{{Kind: "time", AmountCents: 1000}}})
	e.core.SendInvoice(old)
	e.tick()
	if n := len(e.entities(qbo.KindInvoice)); n != 0 {
		t.Fatalf("%d invoices reached QBO", n)
	}
	if r := e.invoiceRow(draft); r.State != "skipped" || r.QBOID != "" {
		t.Fatalf("draft %+v", r)
	}
	if r := e.invoiceRow(old); r.State != "skipped" || !strings.Contains(r.SkipReason, "start date 2026-01-01") {
		t.Fatalf("old %+v", r)
	}
}

func TestMODQBO09TrustPaymentSkipped(t *testing.T) {
	e := newEnv(t)
	pid := e.core.RecordPayment(fakecore.PaymentInput{MatterID: e.matter, AmountCents: 80000, Destination: "trust", TrustAccountID: e.trust})
	e.tick()
	if r := e.paymentRow(pid); r.State != "skipped" || r.SkipReason != "posted from trust ledger entry" {
		t.Fatalf("payment %+v", r)
	}
	if n := len(e.entities(qbo.KindPayment)); n != 0 {
		t.Fatalf("%d payments in QBO", n)
	}
	if jes := e.entities(qbo.KindJournalEntry); len(jes) != 1 {
		t.Fatalf("%d journal entries", len(jes))
	}
	if b := e.qb.Balances(realmA)["36"]; b != 80000 {
		t.Fatalf("trust bank %d", b)
	}
}

// applied checks the pair an apply makes: the two-line trust entry and the payment into
// operating applied to the invoice.
func applied(t *testing.T, e *env, inv, entry, pay string, cents int64) {
	t.Helper()
	cust := e.customerID(e.jane)
	je := journal(t, e.qb.Get(realmA, qbo.KindJournalEntry, e.trustRow(entry).QBOID))
	want := []string{"Debit 90 " + itoa(cents) + " customer " + cust, "Credit 36 " + itoa(cents)}
	if got := lineSet(je); !equal(got, want) {
		t.Fatalf("lines %v, want %v", got, want)
	}
	pr := e.paymentRow(pay)
	p := e.qb.Get(realmA, qbo.KindPayment, pr.QBOID).(*qbo.Payment)
	if int64(p.TotalAmt) != cents || p.DepositToAccountRef.Value != "35" || len(p.Line) != 1 || p.Line[0].LinkedTxn[0].TxnID != e.invoiceRow(inv).QBOID {
		t.Fatalf("payment %+v", p)
	}
}

func TestMODQBO10ApplyTrust(t *testing.T) {
	e := newEnv(t)
	inv := e.sentInvoice(e.matter, "INV-2001", 80000)
	e.core.PostTrust(e.matter, e.trust, "deposit", 100000, "Retainer")
	e.tick()
	before := e.qb.InvoiceBalance(realmA, e.invoiceRow(inv).QBOID)
	entry, pay := e.core.ApplyTrust(inv, e.trust, 50000)
	e.tick()
	applied(t, e, inv, entry, pay, 50000)
	if after := e.qb.InvoiceBalance(realmA, e.invoiceRow(inv).QBOID); int64(before-after) != 50000 || int64(after) != e.core.Invoice(inv).BalanceDueCents {
		t.Fatalf("balance %v to %v", before, after)
	}
	if r := e.trustRow(entry); r.State != "synced" {
		t.Fatalf("entry %+v", r)
	}
}

func TestMODQBO11FlatFeeApply(t *testing.T) {
	e := newEnv(t)
	inv := e.core.CreateInvoice(fakecore.InvoiceInput{MatterID: e.matter, Number: "FF-1", IssuedOn: "2026-03-01",
		Lines: []quickbooks.InvoiceLine{{Kind: "flat_fee", Description: "Estate plan", AmountCents: 150000}}})
	e.core.SendInvoice(inv)
	e.core.PostTrust(e.matter, e.trust, "deposit", 150000, "Flat fee deposit")
	entry, pay := e.core.ApplyTrust(inv, e.trust, 150000)
	e.tick()
	applied(t, e, inv, entry, pay, 150000)
	if b := e.qb.InvoiceBalance(realmA, e.invoiceRow(inv).QBOID); b != 0 {
		t.Fatalf("balance %v", b)
	}
}

func TestMODQBO12TransferNoInvoice(t *testing.T) {
	e := newEnv(t)
	e.core.PostTrust(e.matter, e.trust, "deposit", 50000, "Retainer")
	tr := e.core.PostTrust(e.matter, e.trust, "transfer_to_operating", 20000, "Earned fees")
	e.tick()
	cust := e.customerID(e.jane)
	je := journal(t, e.qb.Get(realmA, qbo.KindJournalEntry, e.trustRow(tr).QBOID))
	want := []string{"Debit 90 20000 customer " + cust, "Credit 36 20000", "Debit 35 20000", "Credit 79 20000"}
	if got := lineSet(je); !equal(got, want) {
		t.Fatalf("lines %v, want %v", got, want)
	}
}

func TestMODQBO13TrustShapes(t *testing.T) {
	e := newEnv(t)
	dep := e.core.PostTrust(e.matter, e.trust, "deposit", 90000, "Retainer")
	wd := e.core.PostTrust(e.matter, e.trust, "withdrawal", 10000, "Court costs")
	rf := e.core.PostTrust(e.matter, e.trust, "refund", 5000, "Refund to client")
	up := e.core.PostTrust(e.matter, e.trust, "adjustment", 300, "Bank correction up")
	down := e.core.PostTrust(e.matter, e.trust, "adjustment", -200, "Bank correction down")
	rev := e.core.Reverse(wd)
	e.tick()
	cust := e.customerID(e.jane)
	c := " customer " + cust
	cases := map[string][]string{
		dep:  {"Debit 36 90000", "Credit 90 90000" + c},
		wd:   {"Debit 90 10000" + c, "Credit 36 10000"},
		rf:   {"Debit 90 5000" + c, "Credit 36 5000"},
		up:   {"Debit 36 300", "Credit 90 300" + c},
		down: {"Debit 90 200" + c, "Credit 36 200"},
		rev:  {"Credit 90 10000" + c, "Debit 36 10000"},
	}
	for id, want := range cases {
		je := journal(t, e.qb.Get(realmA, qbo.KindJournalEntry, e.trustRow(id).QBOID))
		if got := lineSet(je); !equal(got, want) {
			t.Fatalf("%s: lines %v, want %v", id, got, want)
		}
	}
	// the reversal mirrors the original exactly
	orig := journal(t, e.qb.Get(realmA, qbo.KindJournalEntry, e.trustRow(wd).QBOID))
	mirror := journal(t, e.qb.Get(realmA, qbo.KindJournalEntry, e.trustRow(rev).QBOID))
	for i := range orig.Line {
		a, b := orig.Line[i].JournalEntryLineDetail, mirror.Line[i].JournalEntryLineDetail
		if a.AccountRef.Value != b.AccountRef.Value || a.PostingType == b.PostingType || orig.Line[i].Amount != mirror.Line[i].Amount {
			t.Fatalf("line %d is not mirrored: %+v %+v", i, a, b)
		}
	}
	if b := e.qb.Balances(realmA)["36"]; b != 90000-10000-5000+300-200+10000 {
		t.Fatalf("trust bank %d", b)
	}
	// no trust journal line touches an income account (no four-line shape here)
	for _, ent := range e.entities(qbo.KindJournalEntry) {
		for _, l := range journal(t, ent).Line {
			if a := l.JournalEntryLineDetail.AccountRef.Value; a == "79" || a == "80" {
				t.Fatalf("income line in %+v", ent)
			}
		}
	}
}

func TestMODQBO14BelowZeroWithdrawal(t *testing.T) {
	e := newEnv(t)
	e.core.PostTrust(e.matter, e.trust, "deposit", 1000, "Small deposit")
	wd := e.core.PostTrust(e.matter, e.trust, "withdrawal", 5000, "Overdrawn withdrawal")
	e.tick()
	if r := e.trustRow(wd); r.State != "synced" {
		t.Fatalf("row %+v", r)
	}
	if b := e.qb.Balances(realmA)["36"]; b != -4000 {
		t.Fatalf("trust bank %d", b)
	}
}

func TestMODQBO15OperatingPayment(t *testing.T) {
	e := newEnv(t)
	inv := e.sentInvoice(e.matter, "INV-3001", 40000)
	e.tick()
	p1 := e.core.RecordPayment(fakecore.PaymentInput{MatterID: e.matter, InvoiceID: inv, AmountCents: 15000, Method: "check", Reference: "CHK-000000000000000000001234"})
	e.tick()
	pm := e.qb.Get(realmA, qbo.KindPayment, e.paymentRow(p1).QBOID).(*qbo.Payment)
	if pm.DepositToAccountRef.Value != "4" || pm.Line[0].LinkedTxn[0].TxnID != e.invoiceRow(inv).QBOID || len(pm.PaymentRefNum) != 21 || pm.PaymentMethodRef == nil {
		t.Fatalf("payment %+v", pm)
	}
	// a payment on an invoice not yet synced waits, then posts after it
	e.core.Settings["payment_deposit"] = "operating_bank"
	inv2 := e.sentInvoice(e.matter, "INV-3002", 25000)
	p2 := e.core.RecordPayment(fakecore.PaymentInput{MatterID: e.matter, InvoiceID: inv2, AmountCents: 25000, Method: "ach"})
	e.qb.FailNext(qbo.KindInvoice, &qbo.Fault{HTTPStatus: 503, Code: "503", Message: "unavailable", Retryable: true})
	e.tick()
	if r := e.paymentRow(p2); r.State != "pending" || r.WaitingOn != "invoice" || r.Attempts != 0 {
		t.Fatalf("waiting payment %+v", r)
	}
	e.clock.Advance(time.Minute)
	e.tick()
	r := e.paymentRow(p2)
	if r.State != "synced" {
		t.Fatalf("payment %+v", r)
	}
	pm2 := e.qb.Get(realmA, qbo.KindPayment, r.QBOID).(*qbo.Payment)
	if pm2.DepositToAccountRef.Value != "35" || pm2.Line[0].LinkedTxn[0].TxnID != e.invoiceRow(inv2).QBOID {
		t.Fatalf("payment %+v", pm2)
	}
}

func TestMODQBO16ReversedPayment(t *testing.T) {
	e := newEnv(t)
	inv := e.sentInvoice(e.matter, "INV-4001", 30000)
	p := e.core.RecordPayment(fakecore.PaymentInput{MatterID: e.matter, InvoiceID: inv, AmountCents: 30000})
	e.tick()
	e.core.ReversePayment(p)
	e.tick()
	if !e.qb.Voided(realmA, qbo.KindPayment, e.paymentRow(p).QBOID) {
		t.Fatal("payment not voided in QBO")
	}
	never := e.core.RecordPayment(fakecore.PaymentInput{MatterID: e.matter, AmountCents: 5000})
	e.core.ReversePayment(never)
	e.qb.ResetCalls()
	e.tick()
	if r := e.paymentRow(never); r.State != "skipped" || r.QBOID != "" {
		t.Fatalf("never synced %+v", r)
	}
	for _, c := range e.qb.Writes() {
		if c.Op == "void" {
			t.Fatalf("unexpected void %+v", c)
		}
	}
}

func TestMODQBO17VoidedInvoice(t *testing.T) {
	e := newEnv(t)
	inv := e.sentInvoice(e.matter, "INV-5001", 60000)
	p := e.core.RecordPayment(fakecore.PaymentInput{MatterID: e.matter, InvoiceID: inv, AmountCents: 20000})
	e.core.WriteOff(inv, 5000, "goodwill")
	e.tick()
	ir := e.invoiceRow(inv)
	e.core.ReversePayment(p)
	e.core.VoidInvoice(inv)
	e.qb.ResetCalls()
	e.tick()
	if !e.qb.Voided(realmA, qbo.KindInvoice, ir.QBOID) || !e.qb.Voided(realmA, qbo.KindPayment, e.paymentRow(p).QBOID) {
		t.Fatal("not voided")
	}
	var order []string
	for _, c := range e.qb.Writes() {
		order = append(order, c.Op+" "+string(c.Kind))
	}
	want := []string{"void Payment", "delete Payment", "delete CreditMemo", "void Invoice"}
	if !equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	// a draft voided before sync makes no QBO call
	d := e.core.CreateInvoice(fakecore.InvoiceInput{MatterID: e.matter, Number: "INV-5002", IssuedOn: "2026-03-01",
		Lines: []quickbooks.InvoiceLine{{Kind: "time", AmountCents: 100}}})
	e.core.VoidInvoice(d)
	e.qb.ResetCalls()
	e.tick()
	if n := e.writes(); n != 0 {
		t.Fatalf("%d writes", n)
	}
	if r := e.invoiceRow(d); r.State != "skipped" {
		t.Fatalf("draft %+v", r)
	}
}

func TestMODQBO18WriteOff(t *testing.T) {
	e := newEnv(t)
	inv := e.sentInvoice(e.matter, "INV-6001", 100000)
	e.tick()
	e.core.WriteOff(inv, 12500, "hardship")
	e.tick()
	r := e.invoiceRow(inv)
	cms := e.entities(qbo.KindCreditMemo)
	if len(cms) != 1 {
		t.Fatalf("%d credit memos", len(cms))
	}
	if b := e.qb.InvoiceBalance(realmA, r.QBOID); int64(b) != e.core.Invoice(inv).BalanceDueCents || b != 87500 {
		t.Fatalf("balance %v", b)
	}
	e.core.WriteOff(inv, 20000, "hardship")
	e.tick()
	cms = e.entities(qbo.KindCreditMemo)
	cm := cms[0].(*qbo.CreditMemo)
	if len(cms) != 1 || int64(cm.Line[0].Amount) != 20000 {
		t.Fatalf("credit memo %+v", cm)
	}
	if b := e.qb.InvoiceBalance(realmA, r.QBOID); int64(b) != e.core.Invoice(inv).BalanceDueCents || b != 80000 {
		t.Fatalf("balance %v", b)
	}
	zero := 0
	for _, ent := range e.entities(qbo.KindPayment) {
		if p := ent.(*qbo.Payment); p.TotalAmt == 0 {
			zero++
		}
	}
	if zero != 1 {
		t.Fatalf("%d zero payments", zero)
	}
}
