package quickbooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/CharitonMedia/togavsu-quickbooks/internal/wire"
)

// CoreError is a non-2xx answer from the core.
type CoreError struct {
	Status int
	Code   string
	Path   string
}

func (e *CoreError) Error() string {
	return fmt.Sprintf("core %s: %d %s", e.Path, e.Status, e.Code)
}

// isNotFound reports a core not_found. For a books reader call it means the record is on
// a walled matter (core money records are never deleted).
func isNotFound(err error) bool {
	var ce *CoreError
	return errors.As(err, &ce) && ce.Status == http.StatusNotFound
}

type core struct {
	base, token string
	hc          *http.Client
	pageLimit   int
}

// do calls the core with the module token and, for a viewer call, the frame's context
// token. With no context token the call is a service call (the books reader).
func (c *core) do(ctx context.Context, method, path, contextToken string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.base, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contextToken != "" {
		req.Header.Set(wire.ContextHeader, contextToken)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var p struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&p)
		return &CoreError{Status: resp.StatusCode, Code: p.Code, Path: strings.SplitN(path, "?", 2)[0]}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type page[T any] struct {
	Data []T `json:"data"`
	Page struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"page"`
}

// listAll reads every page of a core list.
func listAll[T any](ctx context.Context, c *core, path string, q url.Values, contextToken string) ([]T, error) {
	if q == nil {
		q = url.Values{}
	}
	q.Set("limit", strconv.Itoa(c.pageLimit))
	var out []T
	for {
		var p page[T]
		if err := c.do(ctx, http.MethodGet, path+"?"+q.Encode(), contextToken, nil, &p); err != nil {
			return nil, err
		}
		out = append(out, p.Data...)
		if p.Page.NextCursor == nil || *p.Page.NextCursor == "" {
			return out, nil
		}
		q.Set("cursor", *p.Page.NextCursor)
	}
}

// Core record shapes (the fields the module reads).

// InvoiceLine is one invoice line.
type InvoiceLine struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
	AmountCents int64  `json:"amount_cents"`
}

// Included is the include= object on a record the viewer read.
type Included map[string][]map[string]any

// Invoice is a core invoice.
type Invoice struct {
	ID              string        `json:"id"`
	MatterID        string        `json:"matter_id"`
	Number          string        `json:"number"`
	IssuedOn        string        `json:"issued_on"`
	Status          string        `json:"status"`
	Lines           []InvoiceLine `json:"lines"`
	AdjustmentCents int64         `json:"adjustment_cents"`
	AdjustmentNote  string        `json:"adjustment_reason"`
	InterestCents   int64         `json:"interest_cents"`
	TotalCents      int64         `json:"total_cents"`
	AmountPaidCents int64         `json:"amount_paid_cents"`
	BalanceDueCents int64         `json:"balance_due_cents"`
	WriteOffCents   int64         `json:"write_off_cents"`
	WriteOffReason  string        `json:"write_off_reason"`
	Included        Included      `json:"included"`
}

// Payment is a core payment.
type Payment struct {
	ID              string    `json:"id"`
	CreatedAt       time.Time `json:"created_at"`
	ContactID       string    `json:"contact_id"`
	MatterID        string    `json:"matter_id"`
	AmountCents     int64     `json:"amount_cents"`
	Destination     string    `json:"destination"`
	Method          string    `json:"method"`
	ReferenceNumber string    `json:"reference_number"`
	Status          string    `json:"status"`
	InvoiceID       *string   `json:"invoice_id"`
	TrustAccountID  *string   `json:"trust_account_id"`
	TrustEntryID    *string   `json:"trust_entry_id"`
	Included        Included  `json:"included"`
}

// TrustEntry is a core trust ledger row.
type TrustEntry struct {
	ID              string    `json:"id"`
	CreatedAt       time.Time `json:"created_at"`
	TrustAccountID  string    `json:"trust_account_id"`
	ClientID        string    `json:"client_id"`
	MatterID        string    `json:"matter_id"`
	EntryType       string    `json:"entry_type"`
	AmountCents     int64     `json:"amount_cents"`
	Method          string    `json:"method"`
	Description     string    `json:"description"`
	ReversesEntryID *string   `json:"reverses_entry_id"`
}

// Signed is the entry's effect on the client's trust balance, in cents.
func (e TrustEntry) Signed() int64 {
	switch e.EntryType {
	case "deposit":
		return e.AmountCents
	case "withdrawal", "refund", "transfer_to_operating":
		return -e.AmountCents
	}
	return e.AmountCents // adjustment and reversal carry their sign
}

// TrustAccount is a firm trust bank account.
type TrustAccount struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Bank     string `json:"bank"`
	LastFour string `json:"last_four"`
	Active   bool   `json:"active"`
}

// Expense is a core expense.
type Expense struct {
	ID          string  `json:"id"`
	MatterID    string  `json:"matter_id"`
	IncurredOn  string  `json:"incurred_on"`
	Description string  `json:"description"`
	AmountCents int64   `json:"amount_cents"`
	Billable    bool    `json:"billable"`
	InvoiceID   *string `json:"invoice_id"`
}

// BooksMatter is GET /api/v1/books/matters/{id}.
type BooksMatter struct {
	ID              string `json:"id"`
	Number          string `json:"number"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	ClientContactID string `json:"client_contact_id"`
}

// BooksAddress is a contact's billing address.
type BooksAddress struct {
	Line1      string `json:"line1"`
	Line2      string `json:"line2"`
	City       string `json:"city"`
	Region     string `json:"region"`
	PostalCode string `json:"postal_code"`
	Country    string `json:"country"`
}

// BooksContact is GET /api/v1/books/contacts/{id}.
type BooksContact struct {
	ID               string        `json:"id"`
	Kind             string        `json:"kind"`
	DisplayName      string        `json:"display_name"`
	GivenName        string        `json:"given_name"`
	FamilyName       string        `json:"family_name"`
	OrganizationName string        `json:"organization_name"`
	Email            string        `json:"email"`
	Phone            string        `json:"phone"`
	BillingAddress   *BooksAddress `json:"billing_address"`
	Archived         bool          `json:"archived"`
}

// Profile is GET /api/v1/books/profile.
type Profile struct {
	FirmName string `json:"firm_name"`
	TimeZone string `json:"time_zone"`
	Currency string `json:"currency"`
}

// Matter is the viewer's GET /api/v1/matters/{id}.
type Matter struct {
	ID     string `json:"id"`
	Number string `json:"number"`
	Name   string `json:"name"`
}

// TrustBalanceRow is one row of GET /api/v1/trust/balances.
type TrustBalanceRow struct {
	Scope          string `json:"scope"`
	TrustAccountID string `json:"trust_account_id"`
	BalanceCents   int64  `json:"balance_cents"`
}

// Viewer is a verified context token.
type Viewer struct {
	UserID   string   `json:"viewer_user_id"`
	Roles    []string `json:"roles"`
	MatterID *string  `json:"matter_id"`
	Token    string   `json:"-"`
}

// Has reports whether the viewer holds any of roles.
func (v Viewer) Has(roles ...string) bool {
	for _, have := range v.Roles {
		for _, want := range roles {
			if have == want {
				return true
			}
		}
	}
	return false
}

func (c *core) verify(ctx context.Context, tok string) (Viewer, error) {
	var v Viewer
	if err := c.do(ctx, http.MethodPost, "/api/v1/modules/"+ID+"/context-tokens/verify", tok, nil, &v); err != nil {
		return Viewer{}, err
	}
	if v.UserID == "" {
		return Viewer{}, errors.New("context token names no viewer")
	}
	v.Token = tok
	return v, nil
}

func (c *core) settings(ctx context.Context) (map[string]string, error) {
	var rows []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/modules/"+ID+"/settings", "", nil, &rows); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out, nil
}

func secretPath(name string) string { return "/api/v1/modules/" + ID + "/secrets/" + name }

// getSecret reads a sealed secret. A missing one is ("", false, nil).
func (c *core) getSecret(ctx context.Context, name string) (string, bool, error) {
	var v struct {
		Value string `json:"value"`
	}
	err := c.do(ctx, http.MethodGet, secretPath(name), "", nil, &v)
	if isNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v.Value, true, nil
}

func (c *core) putSecret(ctx context.Context, name, value string) error {
	return c.do(ctx, http.MethodPut, secretPath(name), "", map[string]string{"value": value}, nil)
}

func (c *core) deleteSecret(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, secretPath(name), "", nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

func (c *core) profile(ctx context.Context) (Profile, error) {
	var p Profile
	err := c.do(ctx, http.MethodGet, "/api/v1/books/profile", "", nil, &p)
	return p, err
}

func (c *core) booksMatter(ctx context.Context, id string) (BooksMatter, error) {
	var m BooksMatter
	err := c.do(ctx, http.MethodGet, "/api/v1/books/matters/"+url.PathEscape(id), "", nil, &m)
	return m, err
}

func (c *core) booksContact(ctx context.Context, id string) (BooksContact, error) {
	var m BooksContact
	err := c.do(ctx, http.MethodGet, "/api/v1/books/contacts/"+url.PathEscape(id), "", nil, &m)
	return m, err
}

func (c *core) invoice(ctx context.Context, id string) (Invoice, error) {
	var v Invoice
	err := c.do(ctx, http.MethodGet, "/api/v1/invoices/"+url.PathEscape(id), "", nil, &v)
	return v, err
}

func (c *core) payment(ctx context.Context, id string) (Payment, error) {
	var v Payment
	err := c.do(ctx, http.MethodGet, "/api/v1/payments/"+url.PathEscape(id), "", nil, &v)
	return v, err
}

func (c *core) trustEntry(ctx context.Context, id string) (TrustEntry, error) {
	var v TrustEntry
	err := c.do(ctx, http.MethodGet, "/api/v1/trust-ledger/"+url.PathEscape(id), "", nil, &v)
	return v, err
}

func (c *core) expense(ctx context.Context, id string) (Expense, error) {
	var v Expense
	err := c.do(ctx, http.MethodGet, "/api/v1/expenses/"+url.PathEscape(id), "", nil, &v)
	return v, err
}

// notify sends one module notification to every active user holding a role.
func (c *core) notify(ctx context.Context, title, link string, roles []string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/notifications", "", map[string]any{
		"kind": "sync_problem", "title": title, "link": link, "recipient_roles": roles}, nil)
}
