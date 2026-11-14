package quickbooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
)

// Pure payload builders and hashing (spec section 5). Nothing here calls QBO or the core.

// Marker is the trace note on every entity the module creates.
func Marker(record, id string) string { return "Togavsu " + record + " " + id }

var markerRe = regexp.MustCompile(`Togavsu contact (\S+)`)

// MarkerContacts lists the Togavsu contact ids named in a customer's notes.
func MarkerContacts(notes string) []string {
	var out []string
	for _, m := range markerRe.FindAllStringSubmatch(notes, -1) {
		out = append(out, m[firstGroup])
	}
	return out
}

// CleanDisplayName removes the colon (QBO's sub-customer separator) and control
// characters, and collapses spaces.
func CleanDisplayName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == ':' || unicode.IsControl(r) {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// BaseDisplayName is the organization name or the full name.
func BaseDisplayName(c BooksContact) string {
	name := c.OrganizationName
	if strings.TrimSpace(name) == "" {
		name = strings.TrimSpace(c.GivenName + " " + c.FamilyName)
	}
	if strings.TrimSpace(name) == "" {
		name = c.DisplayName
	}
	return CleanDisplayName(name)
}

// SuffixedName is the name used when a vendor or employee holds the plain one.
func SuffixedName(base, contactID string) string {
	short := strings.ReplaceAll(contactID, "-", "")
	if len(short) > suffixChars {
		short = short[:suffixChars]
	}
	return base + " (" + short + ")"
}

// BuildCustomer is the customer payload under displayName.
func BuildCustomer(c BooksContact, displayName string) *qbo.Customer {
	cu := &qbo.Customer{DisplayName: displayName, GivenName: c.GivenName, FamilyName: c.FamilyName,
		CompanyName: c.OrganizationName, Notes: Marker("contact", c.ID)}
	if c.Email != "" {
		cu.PrimaryEmailAddr = &qbo.EmailAddr{Address: c.Email}
	}
	if c.Phone != "" {
		cu.PrimaryPhone = &qbo.Phone{FreeFormNumber: c.Phone}
	}
	if a := c.BillingAddress; a != nil {
		cu.BillAddr = &qbo.Address{Line1: a.Line1, Line2: a.Line2, City: a.City, CountrySubDivisionCode: a.Region,
			PostalCode: a.PostalCode, Country: a.Country}
	}
	return cu
}

// Refs is the resolved mapping: QBO accounts and items by role.
type Refs struct {
	OperatingBank   qbo.Account
	TrustBank       qbo.Account // the default
	TrustBankFor    map[string]qbo.Account
	TrustLiability  qbo.Account
	EarnedFromTrust qbo.Account
	Undeposited     qbo.Account
	ClientCosts     qbo.Account
	PaidFrom        qbo.Account
	Items           map[string]qbo.Item
	Methods         map[string]string // QBO payment method name -> id
}

func acctRef(a qbo.Account) qbo.RefVal { return qbo.RefVal{Value: a.ID, Name: a.Name} }

func itemRef(i qbo.Item) qbo.RefVal { return qbo.RefVal{Value: i.ID, Name: i.Name} }

// ErrTotals is an invoice whose lines don't add up to its total.
type ErrTotals struct{ Lines, Total int64 }

func (e *ErrTotals) Error() string {
	return fmt.Sprintf("totals_mismatch: lines, adjustment and interest add up to %s but the invoice total is %s",
		qbo.Money(e.Lines), qbo.Money(e.Total))
}

var lineItem = map[string]string{"time": itemLegal, "flat_fee": itemFlatFee, "expense": itemReimbursed, "adjustment": itemAdjustment}

func salesLine(amount int64, desc string, it qbo.Item) qbo.Line {
	return qbo.Line{Amount: qbo.Money(amount), Description: desc, DetailType: "SalesItemLineDetail",
		SalesItemLineDetail: &qbo.SalesItemLineDetail{ItemRef: itemRef(it)}}
}

// BuildInvoice is the invoice payload. Lines plus adjustment plus interest must equal the
// total to the cent.
func BuildInvoice(inv Invoice, matterNumber, customerID string, r Refs) (*qbo.Invoice, error) {
	out := &qbo.Invoice{CustomerRef: qbo.RefVal{Value: customerID}, DocNumber: inv.Number, TxnDate: inv.IssuedOn,
		PrivateNote: Marker("invoice", inv.ID) + ", matter " + matterNumber, EmailStatus: "NotSet"}
	var sum int64
	for _, l := range inv.Lines {
		key, ok := lineItem[l.Kind]
		if !ok {
			return nil, fmt.Errorf("invoice line kind %q has no item", l.Kind)
		}
		out.Line = append(out.Line, salesLine(l.AmountCents, l.Description, r.Items[key]))
		sum += l.AmountCents
	}
	if inv.AdjustmentCents != zero64 {
		desc := "Invoice adjustment"
		if inv.AdjustmentNote != "" {
			desc += ": " + inv.AdjustmentNote
		}
		out.Line = append(out.Line, salesLine(inv.AdjustmentCents, desc, r.Items[itemAdjustment]))
		sum += inv.AdjustmentCents
	}
	if inv.InterestCents != zero64 {
		out.Line = append(out.Line, salesLine(inv.InterestCents, "Late interest", r.Items[itemInterest]))
		sum += inv.InterestCents
	}
	if sum != inv.TotalCents {
		return nil, &ErrTotals{Lines: sum, Total: inv.TotalCents}
	}
	return out, nil
}

// BuildCreditMemo is a write-off's credit memo.
func BuildCreditMemo(inv Invoice, customerID, date string, r Refs) *qbo.CreditMemo {
	desc := "Write-off"
	if inv.WriteOffReason != "" {
		desc += ": " + inv.WriteOffReason
	}
	return &qbo.CreditMemo{CustomerRef: qbo.RefVal{Value: customerID}, TxnDate: date,
		Line:        []qbo.Line{salesLine(inv.WriteOffCents, desc, r.Items[itemWriteOff])},
		PrivateNote: Marker("invoice", inv.ID) + " write-off"}
}

// BuildWriteOffLink is the zero payment that applies the credit memo to the invoice.
func BuildWriteOffLink(inv Invoice, customerID, date, invoiceQBO, creditMemoQBO string) *qbo.Payment {
	amt := qbo.Money(inv.WriteOffCents)
	return &qbo.Payment{CustomerRef: qbo.RefVal{Value: customerID}, TxnDate: date,
		Line: []qbo.Line{
			{Amount: amt, LinkedTxn: []qbo.LinkedTxn{{TxnID: invoiceQBO, TxnType: "Invoice"}}},
			{Amount: amt, LinkedTxn: []qbo.LinkedTxn{{TxnID: creditMemoQBO, TxnType: "CreditMemo"}}},
		},
		PrivateNote: Marker("invoice", inv.ID) + " write-off applied"}
}

// methodNames maps core payment methods to QBO payment method names.
var methodNames = map[string]string{"cash": "Cash", "check": "Check", "card": "Credit Card", "ach": "ACH"}

// BuildPayment is an operating payment. invoiceQBO is empty for an unapplied payment.
func BuildPayment(p Payment, customerID, date, invoiceQBO, depositMode string, r Refs) *qbo.Payment {
	out := &qbo.Payment{CustomerRef: qbo.RefVal{Value: customerID}, TotalAmt: qbo.Money(p.AmountCents), TxnDate: date,
		PrivateNote: Marker("payment", p.ID)}
	ref := p.ReferenceNumber
	if n := limits().PaymentRefChars; len([]rune(ref)) > n {
		ref = string([]rune(ref)[:n])
	}
	out.PaymentRefNum = ref
	if name, ok := methodNames[p.Method]; ok {
		if id, ok := r.Methods[name]; ok {
			out.PaymentMethodRef = &qbo.RefVal{Value: id, Name: name}
		}
	}
	dep := r.Undeposited
	if p.Method == "internal_transfer" || depositMode == "operating_bank" {
		dep = r.OperatingBank
	}
	ar := acctRef(dep)
	out.DepositToAccountRef = &ar
	if invoiceQBO != "" {
		out.Line = []qbo.Line{{Amount: qbo.Money(p.AmountCents), LinkedTxn: []qbo.LinkedTxn{{TxnID: invoiceQBO, TxnType: "Invoice"}}}}
	}
	return out
}

func jeLine(side string, amount int64, a qbo.Account, customerID, desc string) qbo.Line {
	d := &qbo.JournalEntryLineDetail{PostingType: side, AccountRef: acctRef(a)}
	if customerID != "" {
		d.Entity = &qbo.JEEntity{Type: "Customer", EntityRef: qbo.RefVal{Value: customerID}}
	}
	return qbo.Line{Amount: qbo.Money(amount), Description: desc, DetailType: "JournalEntryLineDetail", JournalEntryLineDetail: d}
}

// Trust journal shapes.
const (
	shapeTwo    = "two_line"
	shapeFour   = "four_line"
	shapeMirror = "mirror"
)

// TrustLines are a ledger entry's journal lines (before any mirror). linked reports a
// transfer to operating that a payment applies to an invoice.
func TrustLines(e TrustEntry, customerID string, bank qbo.Account, linked bool, r Refs) ([]qbo.Line, string) {
	desc := e.Description
	in := func(amt int64) []qbo.Line { // money into trust
		return []qbo.Line{jeLine("Debit", amt, bank, "", desc), jeLine("Credit", amt, r.TrustLiability, customerID, desc)}
	}
	out := func(amt int64) []qbo.Line { // money out of trust
		return []qbo.Line{jeLine("Debit", amt, r.TrustLiability, customerID, desc), jeLine("Credit", amt, bank, "", desc)}
	}
	switch e.EntryType {
	case "deposit":
		return in(e.AmountCents), shapeTwo
	case "withdrawal", "refund":
		return out(e.AmountCents), shapeTwo
	case "adjustment":
		if e.AmountCents > zero64 {
			return in(e.AmountCents), shapeTwo
		}
		return out(-e.AmountCents), shapeTwo
	case "transfer_to_operating":
		lines := out(e.AmountCents)
		if linked {
			return lines, shapeTwo
		}
		return append(lines, jeLine("Debit", e.AmountCents, r.OperatingBank, "", desc),
			jeLine("Credit", e.AmountCents, r.EarnedFromTrust, "", desc)), shapeFour
	}
	return nil, ""
}

// Mirror swaps every debit and credit.
func Mirror(lines []qbo.Line) []qbo.Line {
	out := make([]qbo.Line, len(lines))
	for i, l := range lines {
		d := *l.JournalEntryLineDetail
		if d.PostingType == "Debit" {
			d.PostingType = "Credit"
		} else {
			d.PostingType = "Debit"
		}
		l.JournalEntryLineDetail = &d
		out[i] = l
	}
	return out
}

// BuildJournalEntry wraps trust lines.
func BuildJournalEntry(e TrustEntry, date string, lines []qbo.Line) *qbo.JournalEntry {
	return &qbo.JournalEntry{TxnDate: date, Line: lines, PrivateNote: Marker("trust entry", e.ID)}
}

// BankNet is a journal entry's debits minus credits on one account.
func BankNet(je *qbo.JournalEntry, accountID string) int64 {
	var n int64
	for _, l := range je.Line {
		d := l.JournalEntryLineDetail
		if d == nil || d.AccountRef.Value != accountID {
			continue
		}
		if d.PostingType == "Debit" {
			n += int64(l.Amount)
		} else {
			n -= int64(l.Amount)
		}
	}
	return n
}

// BuildPurchase is an expense in purchases mode.
func BuildPurchase(x Expense, customerID string, r Refs) *qbo.Purchase {
	pt := "Cash"
	if r.PaidFrom.AccountType == "Credit Card" {
		pt = "CreditCard"
	}
	billable := "NotBillable"
	if x.Billable {
		billable = "Billable"
	}
	return &qbo.Purchase{AccountRef: acctRef(r.PaidFrom), PaymentType: pt, TxnDate: x.IncurredOn, PrivateNote: Marker("expense", x.ID),
		Line: []qbo.Line{{Amount: qbo.Money(x.AmountCents), Description: x.Description, DetailType: "AccountBasedExpenseLineDetail",
			AccountBasedExpenseLineDetail: &qbo.AccountBasedExpenseLineDetail{AccountRef: acctRef(r.ClientCosts),
				CustomerRef: &qbo.RefVal{Value: customerID}, BillableStatus: billable}}}}
}

// Hash is the SHA-256 of the canonical payload, without the id and SyncToken.
func Hash(e qbo.Entity) string {
	ref := e.GetRef()
	e.SetRef(qbo.Ref{})
	raw, err := json.Marshal(e)
	e.SetRef(ref)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Account type rules (spec section 5).
var (
	bankTypes       = []string{"Bank"}
	liabilityTypes  = []string{"Other Current Liability"}
	incomeTypes     = []string{"Income"}
	clientCostTypes = []string{"Expense", "Other Expense", "Cost of Goods Sold", "Other Current Asset"}
	paidFromTypes   = []string{"Bank", "Credit Card"}
	itemTypes       = []string{"Service", "NonInventory"}
)

func typeOK(t string, allowed []string) bool {
	for _, a := range allowed {
		if a == t {
			return true
		}
	}
	return false
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ResolveAccount finds a mapping value (an Id or the exact full name).
func ResolveAccount(value string, list []qbo.Account) (qbo.Account, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return qbo.Account{}, false
	}
	for _, a := range list {
		if (allDigits(value) && a.ID == value) || a.FullyQualifiedName == value || (a.FullyQualifiedName == "" && a.Name == value) {
			return a, true
		}
	}
	return qbo.Account{}, false
}

// ResolveItem finds an item mapping value.
func ResolveItem(value string, list []qbo.Item) (qbo.Item, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return qbo.Item{}, false
	}
	for _, i := range list {
		if (allDigits(value) && i.ID == value) || i.FullyQualifiedName == value || (i.FullyQualifiedName == "" && i.Name == value) {
			return i, true
		}
	}
	return qbo.Item{}, false
}

// MappingCheck is one mapping setting's resolution, for the setup tab and the push.
type MappingCheck struct {
	Key, Label, Value string
	Found             bool
	ID, Name, Type    string
	Allowed           []string
	TypeOK            bool
}

// OK reports a mapping that resolved to an allowed type.
func (m MappingCheck) OK() bool { return m.Found && m.TypeOK }
