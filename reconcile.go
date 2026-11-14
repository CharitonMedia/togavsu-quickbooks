package quickbooks

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
)

// Run is one reconcile's counts.
type Run struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Examined int    `json:"examined"`
	Queued   int    `json:"queued"`
	Failed   int    `json:"failed"`
	Drift    int    `json:"drift"`
}

// seen is the full read through the books reader: every record id it returns, and
// which of them are dated on or after the start date and in scope.
type seen struct {
	all   map[*table]map[string]bool
	scope map[*table]map[string]bool
}

// fullRead lists every invoice, payment, trust entry and expense the books reader
// returns. q narrows the lists (a matter on the matter tab).
func (s *Server) fullRead(ctx context.Context, p *pass, q url.Values) (*seen, error) {
	sn := &seen{all: map[*table]map[string]bool{}, scope: map[*table]map[string]bool{}}
	for _, t := range []*table{tInvoice, tPayment, tTrust, tExpense} {
		sn.all[t], sn.scope[t] = map[string]bool{}, map[string]bool{}
	}
	vals := func() url.Values {
		v := url.Values{}
		for k, x := range q {
			v[k] = x
		}
		return v
	}
	invs, err := listAll[Invoice](ctx, s.core, "/api/v1/invoices", vals(), "")
	if err != nil {
		return nil, err
	}
	for _, x := range invs {
		sn.all[tInvoice][x.ID] = true
		if x.Status != "draft" && !p.beforeStart(x.IssuedOn) {
			sn.scope[tInvoice][x.ID] = true
		}
	}
	pays, err := listAll[Payment](ctx, s.core, "/api/v1/payments", vals(), "")
	if err != nil {
		return nil, err
	}
	for _, x := range pays {
		sn.all[tPayment][x.ID] = true
		if !p.beforeStart(p.localDate(x.CreatedAt)) && x.Status != "pending" && x.Status != "failed" {
			sn.scope[tPayment][x.ID] = true
		}
	}
	ents, err := listAll[TrustEntry](ctx, s.core, "/api/v1/trust-ledger", vals(), "")
	if err != nil {
		return nil, err
	}
	for _, x := range ents {
		sn.all[tTrust][x.ID] = true
		if !p.beforeStart(p.localDate(x.CreatedAt)) {
			sn.scope[tTrust][x.ID] = true
		}
	}
	exps, err := listAll[Expense](ctx, s.core, "/api/v1/expenses", vals(), "")
	if err != nil {
		return nil, err
	}
	for _, x := range exps {
		sn.all[tExpense][x.ID] = true
		if p.st.ExpensePush == "purchases" && !p.beforeStart(x.IncurredOn) {
			sn.scope[tExpense][x.ID] = true
		}
	}
	return sn, nil
}

// hide moves a row out of view: no QBO call while the wall stands.
func (s *Server) hide(ctx context.Context, r *Row, now int64) error {
	r.State, r.WaitingOn, r.Attempts, r.LastErrorCode, r.LastError, r.UpdatedAt = stateHidden, "", zero, "", "", now
	return s.store.save(ctx, r)
}

// confirm hides every row of t whose record the full read did not return, and returns
// the rows it did.
func (s *Server) confirm(ctx context.Context, sn *seen, t *table, rows []*Row, now int64) ([]*Row, error) {
	var out []*Row
	for _, r := range rows {
		if sn.all[t][r.Key] {
			out = append(out, r)
			continue
		}
		if r.State != stateHidden {
			if err := s.hide(ctx, r, now); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// confirmCustomers hides customer rows whose contact the books reader no longer
// returns, and brings back hidden ones it returns again.
func (s *Server) confirmCustomers(ctx context.Context, rows []*Row, now int64, requeue bool) ([]*Row, error) {
	var out []*Row
	for _, r := range rows {
		_, err := s.core.booksContact(ctx, r.ContactID)
		if isNotFound(err) {
			if r.State != stateHidden {
				if err := s.hide(ctx, r, now); err != nil {
					return nil, err
				}
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if requeue && r.State == stateHidden {
			if err := s.store.queue(ctx, tCustomer, r.Key, now); err != nil {
				return nil, err
			}
			r.State = statePending
		}
		out = append(out, r)
	}
	return out, nil
}

// reconcile is the nightly or manual run: full read, push, drift check, totals check.
// The caller holds s.work.
func (s *Server) reconcile(ctx context.Context, kind, actor string) (Run, error) {
	p, err := s.newPass(ctx, false)
	if err != nil {
		return Run{}, err
	}
	run := Run{ID: newUUIDv7(p.now), Kind: kind}
	var startedBy any
	if actor != "" {
		startedBy = actor
	}
	var lastStart sql.NullInt64
	if err := s.c.DB.QueryRowContext(ctx, `SELECT max(started_at) FROM runs WHERE finished_at IS NOT NULL`).Scan(&lastStart); err != nil {
		return run, err
	}
	if _, err := s.c.DB.ExecContext(ctx, `INSERT INTO runs (id, kind, started_at, started_by) VALUES ($1, $2, $3, $4)`,
		run.ID, kind, p.now.UnixMicro(), startedBy); err != nil {
		return run, err
	}
	now := p.now.UnixMicro()
	sn, err := s.fullRead(ctx, p, nil)
	if err != nil {
		return run, err
	}
	for _, t := range []*table{tInvoice, tPayment, tTrust, tExpense} {
		rows, err := s.store.rows(ctx, t, "")
		if err != nil {
			return run, err
		}
		have := map[string]*Row{}
		for _, r := range rows {
			have[r.Key] = r
		}
		if _, err := s.confirm(ctx, sn, t, rows, now); err != nil {
			return run, err
		}
		for id := range sn.scope[t] {
			run.Examined++
			r := have[id]
			switch {
			case r != nil && r.State == stateFailed:
				// the nightly run tries a failed row once more
				r.State, r.Attempts, r.NextAttemptAt, r.UpdatedAt = statePending, len(p.st.RetrySchedule)-1, now, now
				if err := s.store.save(ctx, r); err != nil {
					return run, err
				}
			case r != nil && r.State == stateDrift:
				continue
			default:
				if err := s.store.queue(ctx, t, id, now); err != nil {
					return run, err
				}
			}
			run.Queued++
		}
	}
	crows, err := s.store.rows(ctx, tCustomer, "")
	if err != nil {
		return run, err
	}
	if _, err := s.confirmCustomers(ctx, crows, now, true); err != nil {
		return run, err
	}
	if p.conn.State == "connected" {
		pp, err := s.newPass(ctx, true)
		if err != nil {
			return run, err
		}
		if err := pp.run(ctx); err != nil {
			return run, err
		}
		since := time.Time{}
		if lastStart.Valid {
			since = time.UnixMicro(lastStart.Int64)
		}
		if err := pp.driftCheck(ctx, since); err != nil {
			return run, err
		}
		if err := pp.totalsCheck(ctx); err != nil {
			return run, err
		}
	}
	for _, t := range tables {
		var f, d int
		if err := s.c.DB.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE state = 'failed'), count(*) FILTER (WHERE state = 'drift') FROM `+t.Name).Scan(&f, &d); err != nil {
			return run, err
		}
		run.Failed += f
		run.Drift += d
	}
	_, err = s.c.DB.ExecContext(ctx, `UPDATE runs SET finished_at = $2, examined = $3, queued = $4, failed = $5, drift = $6 WHERE id = $1`,
		run.ID, s.now().UnixMicro(), run.Examined, run.Queued, run.Failed, run.Drift)
	if p.conn.State != "connected" {
		return run, errPaused
	}
	return run, err
}

// nightlyDue reports whether the nightly run's hour has come and today's has not run.
func (s *Server) nightlyDue(ctx context.Context, st Settings) (bool, error) {
	now := s.now()
	if now.Hour() != st.ReconcileHour {
		return false, nil
	}
	hourStart := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, time.UTC)
	var n int
	err := s.c.DB.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE kind = 'nightly' AND started_at >= $1`, hourStart.UnixMicro()).Scan(&n)
	return n == zero, err
}

// driftTarget is a synced entity the drift check watches.
type driftTarget struct {
	r    *Row
	kind qbo.Kind
	id   string
	tok  string
	what string
}

func (p *pass) driftTargets(ctx context.Context) ([]driftTarget, error) {
	var out []driftTarget
	kinds := map[*table]qbo.Kind{tInvoice: qbo.KindInvoice, tPayment: qbo.KindPayment, tTrust: qbo.KindJournalEntry, tExpense: qbo.KindPurchase}
	for _, t := range []*table{tInvoice, tPayment, tTrust, tExpense} {
		rows, err := p.s.store.rows(ctx, t, "state = 'synced' AND qbo_id <> ''")
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if r.Voided {
				continue
			}
			out = append(out, driftTarget{r: r, kind: kinds[t], id: r.QBOID, tok: r.SyncToken, what: string(kinds[t])})
			if t == tInvoice && r.CreditMemoID != "" {
				out = append(out, driftTarget{r: r, kind: qbo.KindCreditMemo, id: r.CreditMemoID, tok: r.CreditMemoTok, what: "write-off credit memo"})
			}
		}
	}
	return out, nil
}

// driftCheck marks drift every synced entity whose SyncToken moved without the module or
// that was deleted in QBO. Customers and hidden rows are not checked.
func (p *pass) driftCheck(ctx context.Context, since time.Time) error {
	targets, err := p.driftTargets(ctx)
	if err != nil || len(targets) == 0 {
		return err
	}
	moved := func(t driftTarget, why string) error {
		t.r.State, t.r.LastErrorCode, t.r.LastError, t.r.UpdatedAt = stateDrift, "drift", fmt.Sprintf("The %s %s in QuickBooks", t.what, why), p.now.UnixMicro()
		if err := p.s.store.save(ctx, t.r); err != nil {
			return err
		}
		return p.s.store.log(ctx, p.now, p.conn.RealmID, t.r.T, t.r.RecordID(), logEntry{Op: "update", Outcome: "drift", Message: t.r.LastError})
	}
	cdc := !since.IsZero() && p.now.Sub(since) < time.Duration(limits().CDCLookbackDays)*day
	if cdc {
		changes, err := p.books.Changes(ctx, []qbo.Kind{qbo.KindInvoice, qbo.KindPayment, qbo.KindCreditMemo, qbo.KindJournalEntry, qbo.KindPurchase}, since)
		if err != nil {
			return err
		}
		last := map[string]qbo.Change{}
		for _, c := range changes {
			last[string(c.Kind)+"/"+c.ID] = c
		}
		for _, t := range targets {
			c, ok := last[string(t.kind)+"/"+t.id]
			if !ok || t.r.State == stateDrift {
				continue
			}
			switch {
			case c.Deleted:
				err = moved(t, "was deleted")
			case c.SyncToken != t.tok:
				err = moved(t, "was changed by someone else")
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	for _, t := range targets {
		if t.r.State == stateDrift {
			continue
		}
		_, ref, err := p.books.Read(ctx, t.kind, t.id)
		if f := qbo.AsFault(err); f != nil && f.Code == qbo.CodeNotFound {
			err = moved(t, "was deleted")
		} else if err == nil && ref.SyncToken != t.tok {
			err = moved(t, "was changed by someone else")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// totalsCheck compares, per trust account, the trust bank nets of the posted journal
// entries with the core ledger amounts of the same entries. Each entry that differs is
// marked drift with code totals_mismatch, naming the account and the difference.
func (p *pass) totalsCheck(ctx context.Context) error {
	rows, err := p.s.store.rows(ctx, tTrust, "state IN ('synced', 'drift') AND qbo_id <> ''")
	if err != nil || len(rows) == 0 {
		return err
	}
	type diff struct {
		r     *Row
		delta int64
	}
	byAcct := map[string][]diff{}
	totals := map[string]int64{}
	names := map[string]string{}
	for _, r := range rows {
		e, err := p.s.core.trustEntry(ctx, r.Key)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		bank := p.trustBank(e.TrustAccountID)
		ent, _, err := p.books.Read(ctx, qbo.KindJournalEntry, r.QBOID)
		if f := qbo.AsFault(err); f != nil && f.Code == qbo.CodeNotFound {
			continue // the drift check reports a deletion
		}
		if err != nil {
			return err
		}
		je, ok := ent.(*qbo.JournalEntry)
		if !ok {
			continue
		}
		delta := BankNet(je, bank.ID) - e.Signed()
		names[e.TrustAccountID] = bank.Name
		totals[e.TrustAccountID] += delta
		if delta != zero64 {
			byAcct[e.TrustAccountID] = append(byAcct[e.TrustAccountID], diff{r, delta})
		}
	}
	for acct, ds := range byAcct {
		if totals[acct] == zero64 && len(ds) == 0 {
			continue
		}
		for _, d := range ds {
			msg := fmt.Sprintf("Trust account %s (QuickBooks %s): QuickBooks differs from the Togavsu ledger by %s; this entry differs by %s",
				acct, names[acct], qbo.Money(totals[acct]), qbo.Money(d.delta))
			d.r.State, d.r.LastErrorCode, d.r.LastError, d.r.UpdatedAt = stateDrift, "totals_mismatch", msg, p.now.UnixMicro()
			if err := p.s.store.save(ctx, d.r); err != nil {
				return err
			}
			if err := p.s.store.log(ctx, p.now, p.conn.RealmID, tTrust, d.r.Key, logEntry{Op: "update", Outcome: "drift", FaultCode: "totals_mismatch", Message: msg}); err != nil {
				return err
			}
		}
	}
	return nil
}

// errNotAllowed is a role check failure.
var errNotAllowed = errors.New("not allowed")
