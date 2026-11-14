// Package qbo is the module's QuickBooks Online adapter. The sync code depends only on
// the Auth and Books interfaces here; http.go talks to Intuit and qbo/fake keeps
// everything in memory for tests. No test reaches Intuit.
package qbo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MinorVersion is pinned on every QBO call.
const MinorVersion = "75"

// Kind is a QBO entity the module writes.
type Kind string

// The entity kinds the module creates.
const (
	KindCustomer     Kind = "Customer"
	KindInvoice      Kind = "Invoice"
	KindPayment      Kind = "Payment"
	KindCreditMemo   Kind = "CreditMemo"
	KindJournalEntry Kind = "JournalEntry"
	KindPurchase     Kind = "Purchase"
)

// Tokens is one OAuth token pair. RealmID is the QBO company the pair belongs to.
type Tokens struct {
	AccessToken      string
	RefreshToken     string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
	RealmID          string
}

// Ref names one QBO entity version.
type Ref struct {
	ID        string
	SyncToken string
}

// Company is the connected company.
type Company struct {
	RealmID string
	Name    string
}

// RefVal is a QBO reference ({"value": id, "name": ...}).
type RefVal struct {
	Value string `json:"value"`
	Name  string `json:"name,omitempty"`
}

// Account is one chart of accounts entry.
type Account struct {
	ID                 string `json:"Id,omitempty"`
	Name               string `json:"Name"`
	FullyQualifiedName string `json:"FullyQualifiedName,omitempty"`
	AccountType        string `json:"AccountType"`
	AccountSubType     string `json:"AccountSubType,omitempty"`
	Active             bool   `json:"Active"`
}

// Item is a product or service item.
type Item struct {
	ID                 string  `json:"Id,omitempty"`
	Name               string  `json:"Name"`
	FullyQualifiedName string  `json:"FullyQualifiedName,omitempty"`
	Type               string  `json:"Type"`
	IncomeAccountRef   *RefVal `json:"IncomeAccountRef,omitempty"`
	Taxable            bool    `json:"Taxable"`
	Active             bool    `json:"Active"`
}

// PaymentMethod is a QBO payment method (Cash, Check, Credit Card, ...).
type PaymentMethod struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

// Change is one entry of the change data capture feed.
type Change struct {
	Kind      Kind
	ID        string
	SyncToken string
	Deleted   bool
	UpdatedAt time.Time
}

// Fault is every adapter error. Retryable faults are retried on the module's schedule.
type Fault struct {
	HTTPStatus int
	Code       string
	Message    string
	Retryable  bool
}

func (f *Fault) Error() string {
	return fmt.Sprintf("quickbooks %d %s: %s", f.HTTPStatus, f.Code, f.Message)
}

// Fault codes the module acts on.
const (
	CodeDuplicateName = "6240"
	CodeBusinessRule  = "6000"
	CodeStaleToken    = "5010"
	CodeNotFound      = "610"
	CodeInvalidGrant  = "invalid_grant"
	CodeUnauthorized  = "401"
	CodeNetwork       = "network"
)

// AsFault returns err as a *Fault; other errors become retryable network faults.
func AsFault(err error) *Fault {
	if err == nil {
		return nil
	}
	var f *Fault
	if errors.As(err, &f) {
		return f
	}
	return &Fault{Code: CodeNetwork, Message: err.Error(), Retryable: true}
}

// RetryableStatus reports whether an HTTP status is retried.
func RetryableStatus(status int) bool {
	switch status {
	case 429, 500, 502, 503, 504:
		return true
	}
	return false
}

// Auth is Intuit OAuth 2.0.
type Auth interface {
	AuthCodeURL(state, redirectURI string) string
	Exchange(ctx context.Context, code, redirectURI string) (Tokens, error)
	Refresh(ctx context.Context, refreshToken string) (Tokens, error)
	Revoke(ctx context.Context, token string) error
}

// Books is one connected company.
type Books interface {
	Company(ctx context.Context) (Company, error)
	Accounts(ctx context.Context) ([]Account, error)
	Items(ctx context.Context) ([]Item, error)
	PaymentMethods(ctx context.Context) ([]PaymentMethod, error)
	CustomerByName(ctx context.Context, displayName string) (*Customer, error)
	Create(ctx context.Context, e Entity, requestID string) (Ref, error)
	Update(ctx context.Context, e Entity, requestID string) (Ref, error)
	Void(ctx context.Context, kind Kind, ref Ref, requestID string) (Ref, error)
	Delete(ctx context.Context, kind Kind, ref Ref, requestID string) error
	Read(ctx context.Context, kind Kind, id string) (Entity, Ref, error)
	Changes(ctx context.Context, kinds []Kind, since time.Time) ([]Change, error)
	CreateAccount(ctx context.Context, a Account, requestID string) (Ref, error)
	CreateItem(ctx context.Context, i Item, requestID string) (Ref, error)
}

// Conn names the company a Books value is bound to.
type Conn struct {
	Environment string // sandbox or production
	RealmID     string
}

// TokenSource hands the adapter an access token, and a fresh one after a 401.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Refresh(ctx context.Context) (string, error)
}

// Provider builds Auth and Books values. The module takes one so tests inject the fake.
type Provider interface {
	Auth(environment, clientID, clientSecret string) Auth
	Open(conn Conn, ts TokenSource) Books
	// BaseURL is the API base address for an environment.
	BaseURL(environment string) string
}

// Money is integer cents that reads and writes as a two-decimal QBO amount. It never
// passes through a float.
type Money int64

// MarshalJSON writes cents as a decimal number such as 1234.50.
func (m Money) MarshalJSON() ([]byte, error) { return []byte(m.String()), nil }

func (m Money) String() string {
	v := int64(m)
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

// UnmarshalJSON reads a decimal amount exactly; more than two non-zero decimals is an
// error rather than a rounding.
func (m *Money) UnmarshalJSON(b []byte) error {
	c, err := ParseMoney(strings.Trim(string(b), `"`))
	if err != nil {
		return err
	}
	*m = c
	return nil
}

// ParseMoney reads "12.5", "-3", "1234.56" into cents exactly.
func ParseMoney(s string) (Money, error) {
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	frac = strings.TrimRight(frac, "0")
	if len(frac) > 2 {
		return 0, fmt.Errorf("qbo: amount %q has more than two decimals", s)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("qbo: amount %q: %w", s, err)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("qbo: amount %q: %w", s, err)
	}
	v := w*100 + f
	if neg {
		v = -v
	}
	return Money(v), nil
}

// Entity is a typed QBO payload.
type Entity interface {
	Kind() Kind
	GetRef() Ref
	SetRef(Ref)
}

// Head is the id and version every entity carries.
type Head struct {
	ID        string `json:"Id,omitempty"`
	SyncToken string `json:"SyncToken,omitempty"`
}

// GetRef returns the entity's id and SyncToken.
func (h *Head) GetRef() Ref { return Ref{ID: h.ID, SyncToken: h.SyncToken} }

// SetRef sets the entity's id and SyncToken.
func (h *Head) SetRef(r Ref) { h.ID, h.SyncToken = r.ID, r.SyncToken }

// Line is one transaction line.
type Line struct {
	ID                            string                         `json:"Id,omitempty"`
	Description                   string                         `json:"Description,omitempty"`
	Amount                        Money                          `json:"Amount"`
	DetailType                    string                         `json:"DetailType,omitempty"`
	SalesItemLineDetail           *SalesItemLineDetail           `json:"SalesItemLineDetail,omitempty"`
	JournalEntryLineDetail        *JournalEntryLineDetail        `json:"JournalEntryLineDetail,omitempty"`
	AccountBasedExpenseLineDetail *AccountBasedExpenseLineDetail `json:"AccountBasedExpenseLineDetail,omitempty"`
	LinkedTxn                     []LinkedTxn                    `json:"LinkedTxn,omitempty"`
}

// SalesItemLineDetail is an invoice or credit memo line's item.
type SalesItemLineDetail struct {
	ItemRef RefVal `json:"ItemRef"`
}

// JournalEntryLineDetail is a journal line's account, side and entity.
type JournalEntryLineDetail struct {
	PostingType string    `json:"PostingType"` // Debit or Credit
	AccountRef  RefVal    `json:"AccountRef"`
	Entity      *JEEntity `json:"Entity,omitempty"`
}

// JEEntity names the customer on a journal line.
type JEEntity struct {
	Type      string `json:"Type"`
	EntityRef RefVal `json:"EntityRef"`
}

// AccountBasedExpenseLineDetail is a purchase line.
type AccountBasedExpenseLineDetail struct {
	AccountRef     RefVal  `json:"AccountRef"`
	CustomerRef    *RefVal `json:"CustomerRef,omitempty"`
	BillableStatus string  `json:"BillableStatus,omitempty"`
}

// LinkedTxn links a payment line to an invoice or credit memo.
type LinkedTxn struct {
	TxnID   string `json:"TxnId"`
	TxnType string `json:"TxnType"`
}

// EmailAddr is a customer's email.
type EmailAddr struct {
	Address string `json:"Address"`
}

// Phone is a customer's phone.
type Phone struct {
	FreeFormNumber string `json:"FreeFormNumber"`
}

// Address is a billing address.
type Address struct {
	Line1                  string `json:"Line1,omitempty"`
	Line2                  string `json:"Line2,omitempty"`
	City                   string `json:"City,omitempty"`
	CountrySubDivisionCode string `json:"CountrySubDivisionCode,omitempty"`
	PostalCode             string `json:"PostalCode,omitempty"`
	Country                string `json:"Country,omitempty"`
}

// Customer is a QBO customer.
type Customer struct {
	Head
	Sparse           bool       `json:"sparse,omitempty"`
	DisplayName      string     `json:"DisplayName"`
	GivenName        string     `json:"GivenName,omitempty"`
	FamilyName       string     `json:"FamilyName,omitempty"`
	CompanyName      string     `json:"CompanyName,omitempty"`
	PrimaryEmailAddr *EmailAddr `json:"PrimaryEmailAddr,omitempty"`
	PrimaryPhone     *Phone     `json:"PrimaryPhone,omitempty"`
	BillAddr         *Address   `json:"BillAddr,omitempty"`
	Notes            string     `json:"Notes,omitempty"`
}

// Kind is Customer.
func (*Customer) Kind() Kind { return KindCustomer }

// Invoice is a QBO invoice. TotalAmt and Balance are set only on reads.
type Invoice struct {
	Head
	CustomerRef        RefVal `json:"CustomerRef"`
	DocNumber          string `json:"DocNumber,omitempty"`
	TxnDate            string `json:"TxnDate,omitempty"`
	Line               []Line `json:"Line"`
	PrivateNote        string `json:"PrivateNote,omitempty"`
	EmailStatus        string `json:"EmailStatus"`
	AllowOnlinePayment bool   `json:"AllowOnlinePayment"`
	AllowOnlineCard    bool   `json:"AllowOnlineCreditCardPayment"`
	AllowOnlineACH     bool   `json:"AllowOnlineACHPayment"`
	TotalAmt           *Money `json:"TotalAmt,omitempty"`
	Balance            *Money `json:"Balance,omitempty"`
}

// Kind is Invoice.
func (*Invoice) Kind() Kind { return KindInvoice }

// Payment is a QBO receive-payment.
type Payment struct {
	Head
	Sparse              bool    `json:"sparse,omitempty"`
	CustomerRef         RefVal  `json:"CustomerRef"`
	TotalAmt            Money   `json:"TotalAmt"`
	TxnDate             string  `json:"TxnDate,omitempty"`
	PaymentRefNum       string  `json:"PaymentRefNum,omitempty"`
	PaymentMethodRef    *RefVal `json:"PaymentMethodRef,omitempty"`
	DepositToAccountRef *RefVal `json:"DepositToAccountRef,omitempty"`
	Line                []Line  `json:"Line,omitempty"`
	PrivateNote         string  `json:"PrivateNote,omitempty"`
	UnappliedAmt        *Money  `json:"UnappliedAmt,omitempty"`
}

// Kind is Payment.
func (*Payment) Kind() Kind { return KindPayment }

// CreditMemo is a QBO credit memo (a write-off).
type CreditMemo struct {
	Head
	CustomerRef     RefVal `json:"CustomerRef"`
	TxnDate         string `json:"TxnDate,omitempty"`
	Line            []Line `json:"Line"`
	PrivateNote     string `json:"PrivateNote,omitempty"`
	TotalAmt        *Money `json:"TotalAmt,omitempty"`
	RemainingCredit *Money `json:"RemainingCredit,omitempty"`
}

// Kind is CreditMemo.
func (*CreditMemo) Kind() Kind { return KindCreditMemo }

// JournalEntry is a QBO journal entry.
type JournalEntry struct {
	Head
	TxnDate     string `json:"TxnDate,omitempty"`
	Line        []Line `json:"Line"`
	PrivateNote string `json:"PrivateNote,omitempty"`
}

// Kind is JournalEntry.
func (*JournalEntry) Kind() Kind { return KindJournalEntry }

// Purchase is a QBO expense (purchases mode).
type Purchase struct {
	Head
	AccountRef  RefVal `json:"AccountRef"`
	PaymentType string `json:"PaymentType"` // Cash or CreditCard
	TxnDate     string `json:"TxnDate,omitempty"`
	Line        []Line `json:"Line"`
	PrivateNote string `json:"PrivateNote,omitempty"`
	TotalAmt    *Money `json:"TotalAmt,omitempty"`
}

// Kind is Purchase.
func (*Purchase) Kind() Kind { return KindPurchase }

// New returns an empty entity of kind, for decoding.
func New(k Kind) (Entity, error) {
	switch k {
	case KindCustomer:
		return &Customer{}, nil
	case KindInvoice:
		return &Invoice{}, nil
	case KindPayment:
		return &Payment{}, nil
	case KindCreditMemo:
		return &CreditMemo{}, nil
	case KindJournalEntry:
		return &JournalEntry{}, nil
	case KindPurchase:
		return &Purchase{}, nil
	}
	return nil, fmt.Errorf("qbo: unknown kind %q", k)
}
