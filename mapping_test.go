package quickbooks

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
)

func testRefs() Refs {
	acct := func(id, name, typ string) qbo.Account { return qbo.Account{ID: id, Name: name, AccountType: typ} }
	return Refs{
		OperatingBank: acct("35", "Operating Checking", "Bank"), TrustBank: acct("36", "IOLTA Trust", "Bank"),
		TrustLiability: acct("90", "Client Trust Liability", "Other Current Liability"), EarnedFromTrust: acct("79", "Legal Fee Income", "Income"),
		Undeposited: acct("4", "Undeposited Funds", "Other Current Asset"), ClientCosts: acct("91", "Client Costs Advanced", "Other Current Asset"),
		PaidFrom: acct("41", "Firm Card", "Credit Card"),
		Items: map[string]qbo.Item{itemLegal: {ID: "1", Name: "Legal Services"}, itemFlatFee: {ID: "2", Name: "Flat Fee"},
			itemReimbursed: {ID: "3", Name: "Reimbursed Costs"}, itemAdjustment: {ID: "4", Name: "Invoice Adjustment"},
			itemInterest: {ID: "5", Name: "Late Interest"}, itemWriteOff: {ID: "6", Name: "Write-Off"}},
		Methods: map[string]string{"Check": "2", "Cash": "1"},
	}
}

func TestDisplayNames(t *testing.T) {
	if got := CleanDisplayName("  Jane:\tSample\x00  Esq "); got != "Jane Sample Esq" {
		t.Fatalf("%q", got)
	}
	if got := BaseDisplayName(BooksContact{GivenName: "Jane", FamilyName: "Sample"}); got != "Jane Sample" {
		t.Fatal(got)
	}
	if got := BaseDisplayName(BooksContact{OrganizationName: "Sample: Law Co", GivenName: "Jane"}); got != "Sample Law Co" {
		t.Fatal(got)
	}
	if got := SuffixedName("Jane Sample", "0190f3c2-aaaa-7bbb-8ccc-dddddddddddd"); got != "Jane Sample (0190f3c2)" {
		t.Fatal(got)
	}
	if ids := MarkerContacts("Togavsu contact a-1\nTogavsu contact b-2"); len(ids) != 2 || ids[1] != "b-2" {
		t.Fatal(ids)
	}
}

func TestBuildInvoiceTotals(t *testing.T) {
	inv := Invoice{ID: "inv1", Number: "INV-1", IssuedOn: "2026-03-01", TotalCents: 1500 + 250 - 100 + 33,
		Lines:           []InvoiceLine{{Kind: "time", AmountCents: 1500}, {Kind: "expense", AmountCents: 250}},
		AdjustmentCents: -100, InterestCents: 33}
	q, err := BuildInvoice(inv, "2026-0042", "58", testRefs())
	if err != nil || len(q.Line) != 4 || q.Line[2].SalesItemLineDetail.ItemRef.Value != "4" || q.Line[3].SalesItemLineDetail.ItemRef.Value != "5" {
		t.Fatalf("%v %+v", err, q)
	}
	if q.EmailStatus != "NotSet" || q.AllowOnlinePayment || q.AllowOnlineCard || q.AllowOnlineACH {
		t.Fatal("the invoice could contact the client")
	}
	inv.TotalCents++
	if _, err := BuildInvoice(inv, "2026-0042", "58", testRefs()); err == nil || !strings.Contains(err.Error(), "totals_mismatch") {
		t.Fatalf("mismatch %v", err)
	}
	inv.Lines = append(inv.Lines, InvoiceLine{Kind: "mystery", AmountCents: 1})
	if _, err := BuildInvoice(inv, "2026-0042", "58", testRefs()); err == nil {
		t.Fatal("unknown line kind accepted")
	}
}

func TestBuildPaymentModes(t *testing.T) {
	r := testRefs()
	p := Payment{ID: "p1", AmountCents: 5000, Method: "check", ReferenceNumber: strings.Repeat("9", 30)}
	got := BuildPayment(p, "58", "2026-03-02", "130", "undeposited_funds", r)
	if got.DepositToAccountRef.Value != "4" || len(got.PaymentRefNum) != 21 || got.PaymentMethodRef.Value != "2" || got.Line[0].LinkedTxn[0].TxnID != "130" {
		t.Fatalf("%+v", got)
	}
	if got := BuildPayment(p, "58", "2026-03-02", "", "operating_bank", r); got.DepositToAccountRef.Value != "35" || got.Line != nil {
		t.Fatalf("%+v", got)
	}
	p.Method = "internal_transfer"
	if got := BuildPayment(p, "58", "2026-03-02", "", "undeposited_funds", r); got.DepositToAccountRef.Value != "35" || got.PaymentMethodRef != nil {
		t.Fatalf("%+v", got)
	}
	p.Method = "ach"
	if got := BuildPayment(p, "58", "2026-03-02", "", "undeposited_funds", r); got.PaymentMethodRef != nil {
		t.Fatal("a method the company lacks was sent")
	}
}

func lines(ls []qbo.Line) []string {
	var out []string
	for _, l := range ls {
		d := l.JournalEntryLineDetail
		s := d.PostingType + " " + d.AccountRef.Value + " " + l.Amount.String()
		if d.Entity != nil {
			s += " c"
		}
		out = append(out, s)
	}
	return out
}

func TestTrustShapes(t *testing.T) {
	r := testRefs()
	cases := []struct {
		kind   string
		amount int64
		linked bool
		want   string
		shape  string
	}{
		{"deposit", 1000, false, "Debit 36 10.00|Credit 90 10.00 c", shapeTwo},
		{"withdrawal", 1000, false, "Debit 90 10.00 c|Credit 36 10.00", shapeTwo},
		{"refund", 1000, false, "Debit 90 10.00 c|Credit 36 10.00", shapeTwo},
		{"adjustment", 1000, false, "Debit 36 10.00|Credit 90 10.00 c", shapeTwo},
		{"adjustment", -1000, false, "Debit 90 10.00 c|Credit 36 10.00", shapeTwo},
		{"transfer_to_operating", 1000, true, "Debit 90 10.00 c|Credit 36 10.00", shapeTwo},
		{"transfer_to_operating", 1000, false, "Debit 90 10.00 c|Credit 36 10.00|Debit 35 10.00|Credit 79 10.00", shapeFour},
	}
	for _, c := range cases {
		got, shape := TrustLines(TrustEntry{EntryType: c.kind, AmountCents: c.amount}, "58", r.TrustBank, c.linked, r)
		if strings.Join(lines(got), "|") != c.want || shape != c.shape {
			t.Fatalf("%s %d: %v %s", c.kind, c.amount, lines(got), shape)
		}
	}
	dep, _ := TrustLines(TrustEntry{EntryType: "deposit", AmountCents: 1000}, "58", r.TrustBank, false, r)
	if strings.Join(lines(Mirror(dep)), "|") != "Credit 36 10.00|Debit 90 10.00 c" {
		t.Fatal(lines(Mirror(dep)))
	}
	je := BuildJournalEntry(TrustEntry{ID: "e1"}, "2026-03-02", dep)
	if BankNet(je, "36") != 1000 || BankNet(je, "90") != -1000 {
		t.Fatal("bank net")
	}
}

func TestHashIgnoresIDAndToken(t *testing.T) {
	a := &qbo.Invoice{CustomerRef: qbo.RefVal{Value: "58"}, Line: []qbo.Line{{Amount: 100}}}
	h := Hash(a)
	a.SetRef(qbo.Ref{ID: "130", SyncToken: "4"})
	if Hash(a) != h || a.ID != "130" {
		t.Fatal("the hash depends on the id")
	}
	a.Line[0].Amount = 101
	if Hash(a) == h {
		t.Fatal("the hash ignores the amount")
	}
	if RequestID("inv1", "create", h) != RequestID("inv1", "create", h) || len(RequestID("inv1", "create", h)) >= 50 {
		t.Fatal("request ids")
	}
}

func TestCentsAreExact(t *testing.T) {
	raw, _ := json.Marshal(qbo.Money(1999999999901))
	if string(raw) != "19999999999.01" {
		t.Fatal(string(raw))
	}
	var m qbo.Money
	if err := json.Unmarshal([]byte("0.07"), &m); err != nil || m != 7 {
		t.Fatal(m, err)
	}
}

func TestAccountTypeChecks(t *testing.T) {
	accounts := []qbo.Account{{ID: "35", Name: "Operating Checking", AccountType: "Bank"}, {ID: "79", Name: "Legal Fee Income", AccountType: "Income"},
		{ID: "90", Name: "Client Trust Liability", AccountType: "Other Current Liability"}}
	items := []qbo.Item{{ID: "1", Name: "Legal Services", Type: "Service"}}
	st := Settings{OperatingBank: "35", TrustBank: "Legal Fee Income", TrustLiability: "Client Trust Liability", EarnedFromTrust: "79",
		Items: map[string]string{itemLegal: "Legal Services"}}
	by := map[string]MappingCheck{}
	for _, c := range checkMapping(st, accounts, items) {
		by[c.Key] = c
	}
	if !by["account_operating_bank"].OK() || !by["account_trust_liability"].OK() || !by[itemLegal].OK() {
		t.Fatal("good mapping refused")
	}
	if by["account_trust_bank"].OK() || !by["account_trust_bank"].Found {
		t.Fatal("an Income trust bank passed")
	}
	if by[itemFlatFee].Found {
		t.Fatal("an unset item resolved")
	}
	if _, ok := ResolveAccount("Operating", accounts); ok {
		t.Fatal("a partial name resolved")
	}
}

func TestParseSettings(t *testing.T) {
	v := map[string]string{"reconcile_hour_utc": "8", "retry_schedule": "1m, 5m,30m", "alert_interval_minutes": "15", "sync_start_date": "2026-01-01"}
	st, err := parseSettings(v)
	if err != nil || len(st.RetrySchedule) != 3 || st.RetrySchedule[2] != 30*time.Minute || st.Environment != "sandbox" {
		t.Fatalf("%v %+v", err, st)
	}
	for k, bad := range map[string]string{"reconcile_hour_utc": "24", "retry_schedule": "soon", "alert_interval_minutes": "0", "sync_start_date": "01/01/2026"} {
		w := map[string]string{}
		for a, b := range v {
			w[a] = b
		}
		w[k] = bad
		if _, err := parseSettings(w); err == nil {
			t.Fatalf("%s=%q accepted", k, bad)
		}
	}
}

func TestPurchase(t *testing.T) {
	p := BuildPurchase(Expense{ID: "x1", AmountCents: 4500, Billable: true, IncurredOn: "2026-03-02"}, "58", testRefs())
	d := p.Line[0].AccountBasedExpenseLineDetail
	if p.PaymentType != "CreditCard" || d.AccountRef.Value != "91" || d.BillableStatus != "Billable" || d.CustomerRef.Value != "58" {
		t.Fatalf("%+v", p)
	}
}
