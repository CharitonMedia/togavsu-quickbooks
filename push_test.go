package quickbooks_test

import (
	"strings"
	"testing"

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
