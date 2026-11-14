package quickbooks

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Links the notifications carry.
const (
	exceptionsLink = "/modules/" + ID + "/tabs/books?status=exceptions"
	setupLink      = "/modules/" + ID + "/tabs/setup"
)

func (s *Server) recordAlert(ctx context.Context, kind string, n int) error {
	now := s.now()
	_, err := s.c.DB.ExecContext(ctx, `INSERT INTO alerts_sent (id, at, kind, item_count) VALUES ($1, $2, $3, $4)`,
		newUUIDv7(now), now.UnixMicro(), kind, n)
	return err
}

// alertConnection tells the admins at once that the connection needs them.
func (s *Server) alertConnection(ctx context.Context, title, kind string) {
	if err := s.core.notify(ctx, title, setupLink, []string{"admin"}); err != nil {
		s.log.Printf("alert: %v", err)
		return
	}
	if err := s.recordAlert(ctx, kind, one); err != nil {
		s.log.Printf("alert: %v", err)
	}
}

// reconnectReminder reminds the admins once, fourteen days before the refresh token
// expires.
func (s *Server) reconnectReminder(ctx context.Context) error {
	conn, err := s.store.connection(ctx)
	if err != nil || conn.State != "connected" || conn.RefreshExpiresAt == zero64 {
		return err
	}
	from := time.UnixMicro(conn.RefreshExpiresAt).Add(-time.Duration(limits().ReminderDays) * day)
	if s.now().Before(from) {
		return nil
	}
	var n int
	if err := s.c.DB.QueryRowContext(ctx, `SELECT count(*) FROM alerts_sent WHERE kind = 'reconnect_soon' AND at >= $1`, from.UnixMicro()).Scan(&n); err != nil || n > zero {
		return err
	}
	s.alertConnection(ctx, fmt.Sprintf("Reconnect QuickBooks before %s", time.UnixMicro(conn.RefreshExpiresAt).UTC().Format(time.DateOnly)), "reconnect_soon")
	return nil
}

// alertFailures sends one sync_problem notification to admins and accountants when new
// exceptions appeared since the last one, at most once per alert interval. Each
// exception is confirmed through the books reader first, so a walled record never
// reaches an alert.
func (s *Server) alertFailures(ctx context.Context, st Settings) error {
	var last sql.NullInt64
	if err := s.c.DB.QueryRowContext(ctx, `SELECT max(at) FROM alerts_sent WHERE kind = 'failures'`).Scan(&last); err != nil {
		return err
	}
	now := s.now()
	if last.Valid && now.Before(time.UnixMicro(last.Int64).Add(st.AlertInterval)) {
		return nil
	}
	var since int64
	if last.Valid {
		since = last.Int64
	}
	n := zero
	for _, t := range tables {
		rows, err := s.store.rows(ctx, t, exceptionWhere+" AND updated_at > $1", since)
		if err != nil {
			return err
		}
		for _, r := range rows {
			ok, err := s.visibleToReader(ctx, r)
			if err != nil {
				return err
			}
			if ok {
				n++
			} else if err := s.hide(ctx, r, now.UnixMicro()); err != nil {
				return err
			}
		}
	}
	if n == zero {
		return nil
	}
	title := "1 QuickBooks sync problem needs a look"
	if n > one {
		title = fmt.Sprintf("%d QuickBooks sync problems need a look", n)
	}
	if err := s.core.notify(ctx, title, exceptionsLink, []string{"admin", "accountant"}); err != nil {
		return err
	}
	return s.recordAlert(ctx, "failures", n)
}

// exceptionWhere selects failed and drift rows, and synced invoices whose payments don't
// cover what Togavsu shows paid.
const exceptionWhere = "(state IN ('failed', 'drift') OR (state = 'synced' AND last_error_code = 'paid_without_payments'))"

// visibleToReader asks the books reader for a row's record.
func (s *Server) visibleToReader(ctx context.Context, r *Row) (bool, error) {
	var err error
	switch r.T {
	case tCustomer:
		_, err = s.core.booksContact(ctx, r.ContactID)
	case tInvoice:
		_, err = s.core.invoice(ctx, r.Key)
	case tPayment:
		_, err = s.core.payment(ctx, r.Key)
	case tTrust:
		_, err = s.core.trustEntry(ctx, r.Key)
	case tExpense:
		_, err = s.core.expense(ctx, r.Key)
	}
	if isNotFound(err) {
		return false, nil
	}
	return err == nil, err
}
