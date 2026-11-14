package quickbooks

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Row states.
const (
	statePending      = "pending"
	stateSynced       = "synced"
	stateSkipped      = "skipped"
	stateFailed       = "failed"
	stateDrift        = "drift"
	stateAcknowledged = "acknowledged"
	stateHidden       = "hidden"
)

// table describes one sync table.
type table struct {
	Kind  string // customer, invoice, payment, trust_entry, expense
	Name  string
	Key   string
	LogFK string // the sync_log column naming the core record
	Extra []string
}

var (
	tCustomer = &table{Kind: "customer", Name: "customer_sync", Key: "id", LogFK: "contact_id",
		Extra: []string{"contact_id", "qbo_display_name", "adopted"}}
	tInvoice = &table{Kind: "invoice", Name: "invoice_sync", Key: "invoice_id", LogFK: "invoice_id",
		Extra: []string{"customer_sync_id", "voided", "credit_memo_qbo_id", "credit_memo_sync_token", "write_off_cents", "write_off_link_qbo_id"}}
	tPayment = &table{Kind: "payment", Name: "payment_sync", Key: "payment_id", LogFK: "payment_id",
		Extra: []string{"trust_entry_id", "voided"}}
	tTrust = &table{Kind: "trust_entry", Name: "trust_entry_sync", Key: "trust_entry_id", LogFK: "trust_entry_id",
		Extra: []string{"linked_payment_id", "shape"}}
	tExpense = &table{Kind: "expense", Name: "expense_sync", Key: "expense_id", LogFK: "expense_id"}
)

// tables lists the sync tables in push order.
var tables = []*table{tCustomer, tInvoice, tPayment, tTrust, tExpense}

func tableFor(kind string) *table {
	for _, t := range tables {
		if t.Kind == kind {
			return t
		}
	}
	return nil
}

// Row is one sync row; the table-specific columns sit beside the shared state columns.
type Row struct {
	T             *table
	Key           string
	RealmID       string
	State         string
	SkipReason    string
	WaitingOn     string
	QBOID         string
	SyncToken     string
	PushedHash    string
	RequestID     string
	Attempts      int
	NextAttemptAt int64
	LastErrorCode string
	LastError     string
	SyncedAt      sql.NullInt64
	UpdatedAt     int64

	// customer_sync
	ContactID   string
	DisplayName string
	Adopted     bool
	// invoice_sync
	CustomerSyncID sql.NullString
	Voided         bool
	CreditMemoID   string
	CreditMemoTok  string
	WriteOffCents  int64
	WriteOffLinkID string
	// payment_sync
	TrustEntryID sql.NullString
	// trust_entry_sync
	LinkedPaymentID sql.NullString
	Shape           string
}

// RecordID is the core record the row belongs to.
func (r *Row) RecordID() string {
	if r.T == tCustomer {
		return r.ContactID
	}
	return r.Key
}

const stateCols = "realm_id, state, skip_reason, waiting_on, qbo_id, qbo_sync_token, pushed_hash, request_id, attempts, next_attempt_at, last_error_code, last_error, synced_at, updated_at"

func (r *Row) stateDest() []any {
	return []any{&r.RealmID, &r.State, &r.SkipReason, &r.WaitingOn, &r.QBOID, &r.SyncToken, &r.PushedHash, &r.RequestID,
		&r.Attempts, &r.NextAttemptAt, &r.LastErrorCode, &r.LastError, &r.SyncedAt, &r.UpdatedAt}
}

func (r *Row) stateVals() []any {
	return []any{r.RealmID, r.State, r.SkipReason, r.WaitingOn, r.QBOID, r.SyncToken, r.PushedHash, r.RequestID,
		r.Attempts, r.NextAttemptAt, r.LastErrorCode, r.LastError, r.SyncedAt, r.UpdatedAt}
}

func (r *Row) extraDest() []any {
	switch r.T {
	case tCustomer:
		return []any{&r.ContactID, &r.DisplayName, &r.Adopted}
	case tInvoice:
		return []any{&r.CustomerSyncID, &r.Voided, &r.CreditMemoID, &r.CreditMemoTok, &r.WriteOffCents, &r.WriteOffLinkID}
	case tPayment:
		return []any{&r.TrustEntryID, &r.Voided}
	case tTrust:
		return []any{&r.LinkedPaymentID, &r.Shape}
	}
	return nil
}

func (r *Row) extraVals() []any {
	switch r.T {
	case tCustomer:
		return []any{r.ContactID, r.DisplayName, r.Adopted}
	case tInvoice:
		return []any{r.CustomerSyncID, r.Voided, r.CreditMemoID, r.CreditMemoTok, r.WriteOffCents, r.WriteOffLinkID}
	case tPayment:
		return []any{r.TrustEntryID, r.Voided}
	case tTrust:
		return []any{r.LinkedPaymentID, r.Shape}
	}
	return nil
}

func selectCols(t *table) string {
	cols := t.Key + ", " + stateCols
	if len(t.Extra) > 0 {
		cols += ", " + strings.Join(t.Extra, ", ")
	}
	return cols
}

type store struct{ db *sql.DB }

func (st *store) rows(ctx context.Context, t *table, where string, args ...any) ([]*Row, error) {
	q := "SELECT " + selectCols(t) + " FROM " + t.Name
	if where != "" {
		q += " WHERE " + where
	}
	q += " ORDER BY " + t.Key
	rs, err := st.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*Row
	for rs.Next() {
		r := &Row{T: t}
		dest := append([]any{&r.Key}, r.stateDest()...)
		dest = append(dest, r.extraDest()...)
		if err := rs.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// row returns one row by key, or nil.
func (st *store) row(ctx context.Context, t *table, key string) (*Row, error) {
	rs, err := st.rows(ctx, t, t.Key+" = $1", key)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	return rs[0], nil
}

// save writes every column of r.
func (st *store) save(ctx context.Context, r *Row) error {
	cols := strings.Split(stateCols, ", ")
	cols = append(cols, r.T.Extra...)
	sets := make([]string, len(cols))
	for i, c := range cols {
		sets[i] = fmt.Sprintf("%s = $%d", c, i+2)
	}
	args := append([]any{r.Key}, r.stateVals()...)
	args = append(args, r.extraVals()...)
	_, err := st.db.ExecContext(ctx, "UPDATE "+r.T.Name+" SET "+strings.Join(sets, ", ")+" WHERE "+r.T.Key+" = $1", args...)
	return err
}

// queue marks a record's row pending and due now, creating it if needed. A new event on
// a failed row resets its attempts. A drift row stays drift until a person decides.
func (st *store) queue(ctx context.Context, t *table, key string, now int64) error {
	return queueOn(ctx, st.db, t, key, now)
}

// execer is a database or a transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// queueOn marks a record due for a push, on db or inside a transaction.
func queueOn(ctx context.Context, db execer, t *table, key string, now int64) error {
	set := `
  state = CASE WHEN ` + t.Name + `.state = 'drift' THEN 'drift' ELSE 'pending' END,
  skip_reason = CASE WHEN ` + t.Name + `.state = 'acknowledged' THEN 'acknowledged' ELSE '' END,
  attempts = CASE WHEN ` + t.Name + `.state = 'failed' THEN 0 ELSE ` + t.Name + `.attempts END,
  next_attempt_at = $2, updated_at = $2`
	if t == tCustomer {
		// a customer row is made with its contact by newCustomerRow; here it only requeues
		_, err := db.ExecContext(ctx, `UPDATE customer_sync SET`+set+` WHERE id = $1`, key, now)
		return err
	}
	_, err := db.ExecContext(ctx, `INSERT INTO `+t.Name+` (`+t.Key+`, state, next_attempt_at, updated_at) VALUES ($1, 'pending', $2, $2)
ON CONFLICT (`+t.Key+`) DO UPDATE SET`+set, key, now)
	return err
}

// customerRows returns the customer rows for a contact.
func (st *store) customerRows(ctx context.Context, contactID string) ([]*Row, error) {
	return st.rows(ctx, tCustomer, "contact_id = $1", contactID)
}

func (st *store) newCustomerRow(ctx context.Context, contactID string, now int64) (*Row, error) {
	id := newUUIDv7(time.UnixMicro(now))
	if _, err := st.db.ExecContext(ctx, `INSERT INTO customer_sync (id, contact_id, state, next_attempt_at, updated_at) VALUES ($1, $2, 'pending', $3, $3)`,
		id, contactID, now); err != nil {
		return nil, err
	}
	return st.row(ctx, tCustomer, id)
}

// logEntry is one sync_log row.
type logEntry struct {
	Op, Outcome, FaultCode, Message, RequestID, Actor string
	HTTPStatus                                        int
}

func (st *store) log(ctx context.Context, at time.Time, realm string, t *table, recordID string, e logEntry) error {
	var actor any
	if e.Actor != "" {
		actor = e.Actor
	}
	_, err := st.db.ExecContext(ctx, `INSERT INTO sync_log (id, at, realm_id, `+t.LogFK+`, operation, outcome, http_status, qbo_fault_code, message, request_id, actor_user_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		newUUIDv7(at), at.UnixMicro(), realm, recordID, e.Op, e.Outcome, e.HTTPStatus, e.FaultCode, e.Message, e.RequestID, actor)
	return err
}

// Connection is the one connection row.
type Connection struct {
	Environment      string
	RealmID          string
	CompanyName      string
	AccessExpiresAt  int64
	RefreshExpiresAt int64
	State            string
	ConnectedBy      sql.NullString
	ConnectedAt      int64
	FirstConnectedOn string
	LastRefreshAt    int64
	LastError        string
}

const connCols = "environment, realm_id, company_name, access_expires_at, refresh_expires_at, state, connected_by, connected_at, first_connected_on, last_refresh_at, last_error"

func (st *store) connection(ctx context.Context) (Connection, error) {
	var c Connection
	err := st.db.QueryRowContext(ctx, "SELECT "+connCols+" FROM connection WHERE id = 1").Scan(&c.Environment, &c.RealmID, &c.CompanyName,
		&c.AccessExpiresAt, &c.RefreshExpiresAt, &c.State, &c.ConnectedBy, &c.ConnectedAt, &c.FirstConnectedOn, &c.LastRefreshAt, &c.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return Connection{Environment: "sandbox", State: "disconnected"}, nil
	}
	return c, err
}

func (st *store) saveConnection(ctx context.Context, c Connection) error {
	_, err := st.db.ExecContext(ctx, `INSERT INTO connection (id, `+connCols+`) VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (id) DO UPDATE SET environment = $1, realm_id = $2, company_name = $3, access_expires_at = $4, refresh_expires_at = $5,
  state = $6, connected_by = $7, connected_at = $8, first_connected_on = $9, last_refresh_at = $10, last_error = $11`,
		c.Environment, c.RealmID, c.CompanyName, c.AccessExpiresAt, c.RefreshExpiresAt, c.State, c.ConnectedBy, c.ConnectedAt,
		c.FirstConnectedOn, c.LastRefreshAt, c.LastError)
	return err
}

// syncRealms lists the realms the sync rows belong to.
func (st *store) syncRealms(ctx context.Context) ([]string, error) {
	var parts []string
	for _, t := range tables {
		parts = append(parts, "SELECT realm_id FROM "+t.Name+" WHERE realm_id <> ''")
	}
	rs, err := st.db.QueryContext(ctx, "SELECT DISTINCT realm_id FROM ("+strings.Join(parts, " UNION ")+") r")
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var s string
		if err := rs.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rs.Err()
}

// reset clears the five sync tables and keeps sync_log.
func (st *store) reset(ctx context.Context) error {
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, name := range []string{"invoice_sync", "payment_sync", "trust_entry_sync", "expense_sync", "customer_sync"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// newUUIDv7 returns a time-ordered UUID.
func newUUIDv7(at time.Time) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	ms := uint64(at.UnixMilli())
	binary.BigEndian.PutUint16(b[0:2], uint16(ms>>32))
	binary.BigEndian.PutUint32(b[2:6], uint32(ms))
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

// requestNamespace is the UUIDv5 namespace of QBO request ids.
var requestNamespace = [16]byte{0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}

// RequestID is the UUIDv5 of the record id, operation and payload hash: 36 characters,
// under Intuit's 50, and the same for the same write.
func RequestID(recordID, op, hash string) string {
	h := sha1.New()
	h.Write(requestNamespace[:])
	h.Write([]byte(recordID + "|" + op + "|" + hash))
	var b [16]byte
	copy(b[:], h.Sum(nil))
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func randomToken() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
