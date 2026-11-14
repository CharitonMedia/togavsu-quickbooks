package fake

import (
	"context"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
)

type ts struct {
	f   *Fake
	tok string
}

func (t *ts) Token(context.Context) (string, error) { return t.tok, nil }
func (t *ts) Refresh(context.Context) (string, error) {
	return t.tok, nil
}

func open(t *testing.T) (*Fake, qbo.Books, func(d time.Duration)) {
	t.Helper()
	f := New()
	now := time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)
	f.SetClock(func() time.Time { return now })
	f.Seed("r1", SampleChart())
	tk, err := f.Auth("sandbox", "client", "secret").Exchange(context.Background(), f.IssueCode("r1"), "https://x/cb")
	if err != nil {
		t.Fatal(err)
	}
	return f, f.Open(qbo.Conn{Environment: "sandbox", RealmID: "r1"}, &ts{f: f, tok: tk.AccessToken}), func(d time.Duration) { now = now.Add(d) }
}

var ctx = context.Background()

func TestDisplayNamesAreUnique(t *testing.T) {
	f, b, _ := open(t)
	f.AddVendor("r1", "Vic Vendor")
	if _, err := b.Create(ctx, &qbo.Customer{DisplayName: "Vic Vendor"}, "a"); qbo.AsFault(err).Code != qbo.CodeDuplicateName {
		t.Fatalf("vendor name: %v", err)
	}
	if _, err := b.Create(ctx, &qbo.Customer{DisplayName: "Jane Sample"}, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Create(ctx, &qbo.Customer{DisplayName: "Jane Sample"}, "c"); qbo.AsFault(err).Code != qbo.CodeDuplicateName {
		t.Fatalf("customer name: %v", err)
	}
}

func TestSyncTokenAndReplay(t *testing.T) {
	_, b, _ := open(t)
	cu := &qbo.Customer{DisplayName: "Jane Sample"}
	ref, err := b.Create(ctx, cu, "same")
	if err != nil {
		t.Fatal(err)
	}
	again, err := b.Create(ctx, &qbo.Customer{DisplayName: "Other Name"}, "same")
	if err != nil || again != ref {
		t.Fatalf("replay %v %+v", err, again)
	}
	cu.SetRef(ref)
	cu.FamilyName = "Sample"
	ref2, err := b.Update(ctx, cu, "u1")
	if err != nil || ref2.SyncToken == ref.SyncToken {
		t.Fatalf("update %v %+v", err, ref2)
	}
	cu.SetRef(ref) // stale
	if _, err := b.Update(ctx, cu, "u2"); qbo.AsFault(err).Code != qbo.CodeStaleToken {
		t.Fatalf("stale: %v", err)
	}
}

func TestJournalEntriesBalanceOnKnownAccounts(t *testing.T) {
	f, b, _ := open(t)
	cust, _ := b.Create(ctx, &qbo.Customer{DisplayName: "Jane Sample"}, "c")
	line := func(side, acct string, amt qbo.Money) qbo.Line {
		d := &qbo.JournalEntryLineDetail{PostingType: side, AccountRef: qbo.RefVal{Value: acct}}
		if acct == "90" {
			d.Entity = &qbo.JEEntity{Type: "Customer", EntityRef: qbo.RefVal{Value: cust.ID}}
		}
		return qbo.Line{Amount: amt, DetailType: "JournalEntryLineDetail", JournalEntryLineDetail: d}
	}
	if _, err := b.Create(ctx, &qbo.JournalEntry{Line: []qbo.Line{line("Debit", "36", 100), line("Credit", "90", 99)}}, "j1"); qbo.AsFault(err).Code != qbo.CodeBusinessRule {
		t.Fatalf("unbalanced: %v", err)
	}
	if _, err := b.Create(ctx, &qbo.JournalEntry{Line: []qbo.Line{line("Debit", "999", 100), line("Credit", "90", 100)}}, "j2"); qbo.AsFault(err).Code != qbo.CodeBusinessRule {
		t.Fatalf("unknown account: %v", err)
	}
	ref, err := b.Create(ctx, &qbo.JournalEntry{Line: []qbo.Line{line("Debit", "36", 100), line("Credit", "90", 100)}}, "j3")
	if err != nil {
		t.Fatal(err)
	}
	if bal := f.Balances("r1"); bal["36"] != 100 || bal["90"] != -100 {
		t.Fatalf("balances %v", bal)
	}
	if _, err := b.Void(ctx, qbo.KindJournalEntry, ref, "v"); err != nil {
		t.Fatal(err)
	}
}

func TestVoidKeepsTheRecord(t *testing.T) {
	f, b, _ := open(t)
	cust, _ := b.Create(ctx, &qbo.Customer{DisplayName: "Jane Sample"}, "c")
	inv, err := b.Create(ctx, &qbo.Invoice{CustomerRef: qbo.RefVal{Value: cust.ID}, Line: []qbo.Line{{Amount: 5000, DetailType: "SalesItemLineDetail",
		SalesItemLineDetail: &qbo.SalesItemLineDetail{ItemRef: qbo.RefVal{Value: "1"}}}}}, "i")
	if err != nil {
		t.Fatal(err)
	}
	pay, err := b.Create(ctx, &qbo.Payment{CustomerRef: qbo.RefVal{Value: cust.ID}, TotalAmt: 5000,
		Line: []qbo.Line{{Amount: 5000, LinkedTxn: []qbo.LinkedTxn{{TxnID: inv.ID, TxnType: "Invoice"}}}}}, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Void(ctx, qbo.KindInvoice, inv, "vi"); err == nil {
		t.Fatal("voided an invoice with a live payment")
	}
	if _, err := b.Void(ctx, qbo.KindPayment, pay, "vp"); err != nil {
		t.Fatal(err)
	}
	cur, curRef, _ := b.Read(ctx, qbo.KindInvoice, inv.ID)
	_ = cur
	if _, err := b.Void(ctx, qbo.KindInvoice, curRef, "vi2"); err != nil {
		t.Fatal(err)
	}
	if !f.Voided("r1", qbo.KindInvoice, inv.ID) {
		t.Fatal("not voided")
	}
	got := f.Get("r1", qbo.KindInvoice, inv.ID).(*qbo.Invoice)
	if got.TotalAmt == nil || *got.TotalAmt != 0 {
		t.Fatalf("voided amounts %+v", got.TotalAmt)
	}
}

func TestTokensExpireAndRotate(t *testing.T) {
	f := New()
	now := time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)
	f.SetClock(func() time.Time { return now })
	f.Seed("r1", SampleChart())
	a := f.Auth("sandbox", "client", "secret")
	tk, _ := a.Exchange(ctx, f.IssueCode("r1"), "cb")
	if !f.AccessValid(tk.AccessToken) {
		t.Fatal("fresh token invalid")
	}
	now = now.Add(61 * time.Minute)
	if f.AccessValid(tk.AccessToken) {
		t.Fatal("token outlived an hour")
	}
	tk2, err := a.Refresh(ctx, tk.RefreshToken)
	if err != nil || f.RefreshValid(tk.RefreshToken) || !f.RefreshValid(tk2.RefreshToken) {
		t.Fatalf("rotation %v", err)
	}
	f.RevokeRefresh()
	if _, err := a.Refresh(ctx, tk2.RefreshToken); qbo.AsFault(err).Code != qbo.CodeInvalidGrant {
		t.Fatalf("revoked refresh: %v", err)
	}
	if _, err := a.Exchange(ctx, "used-or-unknown", "cb"); err == nil {
		t.Fatal("unknown code accepted")
	}
}

func TestHooksAndChanges(t *testing.T) {
	f, b, advance := open(t)
	since := time.Date(2026, 3, 2, 14, 0, 0, 0, time.UTC)
	ref, _ := b.Create(ctx, &qbo.Customer{DisplayName: "Jane Sample"}, "c")
	f.FailNext(qbo.KindCustomer, &qbo.Fault{HTTPStatus: 503, Code: "503", Retryable: true})
	if _, err := b.Create(ctx, &qbo.Customer{DisplayName: "Other"}, "c2"); qbo.AsFault(err).Code != "503" {
		t.Fatalf("FailNext: %v", err)
	}
	f.Timeout(qbo.KindCustomer)
	if _, err := b.Create(ctx, &qbo.Customer{DisplayName: "Timed Out"}, "c3"); err == nil {
		t.Fatal("Timeout returned no error")
	}
	if again, err := b.Create(ctx, &qbo.Customer{DisplayName: "Timed Out"}, "c3"); err != nil || again.ID == "" {
		t.Fatalf("replay after timeout: %v", err)
	}
	advance(time.Minute)
	f.EditAsAccountant("r1", qbo.KindCustomer, ref.ID)
	ch, err := b.Changes(ctx, []qbo.Kind{qbo.KindCustomer}, since)
	if err != nil || len(ch) == 0 {
		t.Fatalf("changes %v %v", ch, err)
	}
	f.DeleteAsAccountant("r1", qbo.KindCustomer, ref.ID)
	if _, _, err := b.Read(ctx, qbo.KindCustomer, ref.ID); qbo.AsFault(err).Code != qbo.CodeNotFound {
		t.Fatalf("deleted read: %v", err)
	}
	f.Throttle(1)
	if _, err := b.Accounts(ctx); !qbo.AsFault(err).Retryable {
		t.Fatalf("throttle: %v", err)
	}
	if n := len(f.Calls()); n == 0 {
		t.Fatal("no calls recorded")
	}
}
