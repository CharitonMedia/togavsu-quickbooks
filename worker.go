package quickbooks

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
)

// Outcomes a push can end in besides success and a QBO fault.
type (
	// waitErr keeps the row pending behind another row; it counts no attempt.
	waitErr struct{ on string }
	// skipErr is a record that never syncs, with the reason shown to the accountant.
	skipErr struct{ reason string }
	// failErr is a module-side failure that no retry can fix (mapping_invalid,
	// totals_mismatch).
	failErr struct{ code, msg string }
)

func (e *waitErr) Error() string { return "waiting on " + e.on }
func (e *skipErr) Error() string { return "skipped: " + e.reason }
func (e *failErr) Error() string { return e.code + ": " + e.msg }

// errHidden is a record the books reader no longer returns (a walled matter).
var errHidden = errors.New("hidden by a matter wall")

const codeMapping = "mapping_invalid"

// pass is one worker or reconcile pass: the settings, the connection and the resolved
// mapping read once, so every row in the pass sees the same configuration.
type pass struct {
	s       *Server
	st      Settings
	conn    Connection
	books   qbo.Books
	now     time.Time
	start   string
	tz      *time.Location
	refs    Refs
	checks  []MappingCheck
	bankMap map[string]string // trust account id -> mapping value
	mapErr  map[string]string // row kind -> what is wrong with its mapping
	wrote   bool

	invoiceVoids []voidJob
	paymentVoids []voidJob
	waited       []*Row
}

type voidJob struct {
	r   *Row
	inv Invoice
	p   Payment
}

// newPass reads the settings and the connection. With chart true it also reads the QBO
// chart of accounts and resolves the mapping, which needs a live connection.
func (s *Server) newPass(ctx context.Context, chart bool) (*pass, error) {
	st, err := s.settings(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := s.store.connection(ctx)
	if err != nil {
		return nil, err
	}
	p := &pass{s: s, st: st, conn: conn, now: s.now(), tz: time.UTC}
	p.start = st.StartDate
	if p.start == "" {
		p.start = conn.FirstConnectedOn
	}
	if prof, err := s.core.profile(ctx); err == nil {
		p.tz = zone(prof.TimeZone)
	} else {
		return nil, err
	}
	if !chart {
		return p, nil
	}
	if conn.State != "connected" {
		return p, errPaused
	}
	p.books = s.c.QBO.Open(qbo.Conn{Environment: conn.Environment, RealmID: conn.RealmID}, &tokenSource{s: s, st: st})
	if err := p.resolve(ctx); err != nil {
		return p, err
	}
	return p, nil
}

// localDate is a timestamp's date in the firm's time zone.
func (p *pass) localDate(t time.Time) string { return t.In(p.tz).Format(time.DateOnly) }

// mapSpec is one mapping setting: what it may resolve to and which rows need it.
type mapSpec struct {
	key, label string
	allowed    []string
	item       bool
	kinds      []string
}

func mapSpecs(st Settings) []mapSpec {
	specs := []mapSpec{
		{"account_operating_bank", "Operating bank", bankTypes, false, []string{"payment", "trust_entry"}},
		{"account_trust_bank", "Trust bank (default)", bankTypes, false, []string{"trust_entry"}},
		{"account_trust_liability", "Trust liability", liabilityTypes, false, []string{"trust_entry"}},
		{"account_earned_from_trust", "Earned from trust", incomeTypes, false, []string{"trust_entry"}},
		{itemLegal, "Item for time lines", itemTypes, true, []string{"invoice"}},
		{itemFlatFee, "Item for flat fee lines", itemTypes, true, []string{"invoice"}},
		{itemReimbursed, "Item for expense lines", itemTypes, true, []string{"invoice"}},
		{itemAdjustment, "Item for adjustments", itemTypes, true, []string{"invoice"}},
		{itemInterest, "Item for interest", itemTypes, true, []string{"invoice"}},
		{itemWriteOff, "Item for write-offs", itemTypes, true, []string{"invoice"}},
	}
	if st.ExpensePush == "purchases" {
		specs = append(specs,
			mapSpec{"account_client_costs", "Client costs", clientCostTypes, false, []string{"expense"}},
			mapSpec{"account_expense_paid_from", "Expenses paid from", paidFromTypes, false, []string{"expense"}})
	}
	return specs
}

func (st Settings) value(key string) string {
	switch key {
	case "account_operating_bank":
		return st.OperatingBank
	case "account_trust_bank":
		return st.TrustBank
	case "account_trust_liability":
		return st.TrustLiability
	case "account_earned_from_trust":
		return st.EarnedFromTrust
	case "account_client_costs":
		return st.ClientCosts
	case "account_expense_paid_from":
		return st.PaidFrom
	}
	return st.Items[key]
}

// checkMapping resolves every mapping value against the chart.
func checkMapping(st Settings, accounts []qbo.Account, items []qbo.Item) []MappingCheck {
	var out []MappingCheck
	for _, sp := range mapSpecs(st) {
		c := MappingCheck{Key: sp.key, Label: sp.label, Value: st.value(sp.key), Allowed: sp.allowed}
		if sp.item {
			if it, ok := ResolveItem(c.Value, items); ok {
				c.Found, c.ID, c.Name, c.Type = true, it.ID, it.Name, it.Type
			}
		} else if a, ok := ResolveAccount(c.Value, accounts); ok {
			c.Found, c.ID, c.Name, c.Type = true, a.ID, a.Name, a.AccountType
		}
		c.TypeOK = c.Found && typeOK(c.Type, sp.allowed)
		out = append(out, c)
	}
	return out
}

func describe(c MappingCheck) string {
	if c.Value == "" {
		return c.Label + " (" + c.Key + ") is not set"
	}
	if !c.Found {
		return fmt.Sprintf("%s (%s) %q is not in the QuickBooks company", c.Label, c.Key, c.Value)
	}
	return fmt.Sprintf("%s (%s) is %s, a %s account; it must be %s", c.Label, c.Key, c.Name, c.Type, strings.Join(c.Allowed, " or "))
}

// resolve reads the chart and fills refs and the per-kind mapping problems.
func (p *pass) resolve(ctx context.Context) error {
	accounts, err := p.books.Accounts(ctx)
	if err != nil {
		return err
	}
	items, err := p.books.Items(ctx)
	if err != nil {
		return err
	}
	methods, err := p.books.PaymentMethods(ctx)
	if err != nil {
		return err
	}
	p.checks = checkMapping(p.st, accounts, items)
	p.mapErr = map[string]string{}
	p.refs = Refs{Items: map[string]qbo.Item{}, Methods: map[string]string{}, TrustBankFor: map[string]qbo.Account{}}
	for _, m := range methods {
		p.refs.Methods[m.Name] = m.ID
	}
	for _, sp := range mapSpecs(p.st) {
		var c MappingCheck
		for _, x := range p.checks {
			if x.Key == sp.key {
				c = x
			}
		}
		if !c.OK() {
			for _, k := range sp.kinds {
				if p.mapErr[k] == "" {
					p.mapErr[k] = describe(c)
				}
			}
			continue
		}
		if sp.item {
			it, _ := ResolveItem(c.Value, items)
			p.refs.Items[sp.key] = it
			continue
		}
		a, _ := ResolveAccount(c.Value, accounts)
		switch sp.key {
		case "account_operating_bank":
			p.refs.OperatingBank = a
		case "account_trust_bank":
			p.refs.TrustBank = a
		case "account_trust_liability":
			p.refs.TrustLiability = a
		case "account_earned_from_trust":
			p.refs.EarnedFromTrust = a
		case "account_client_costs":
			p.refs.ClientCosts = a
		case "account_expense_paid_from":
			p.refs.PaidFrom = a
		}
	}
	for _, a := range accounts {
		if a.AccountSubType == "UndepositedFunds" || (p.refs.Undeposited.ID == "" && a.Name == "Undeposited Funds") {
			p.refs.Undeposited = a
		}
	}
	if p.refs.Undeposited.ID == "" && p.st.PaymentDeposit != "operating_bank" && p.mapErr["payment"] == "" {
		p.mapErr["payment"] = "The QuickBooks company has no Undeposited Funds account; set payment_deposit to operating_bank"
	}
	bm, err := p.s.trustBankMap(ctx)
	if err != nil {
		return err
	}
	p.bankMap = bm
	for id, v := range bm {
		a, ok := ResolveAccount(v, accounts)
		if !ok || !typeOK(a.AccountType, bankTypes) {
			if p.mapErr["trust_entry"] == "" {
				p.mapErr["trust_entry"] = fmt.Sprintf("The trust bank mapped for trust account %s (%q) is not a Bank account in QuickBooks", id, v)
			}
			continue
		}
		p.refs.TrustBankFor[id] = a
	}
	return nil
}

func (s *Server) trustBankMap(ctx context.Context) (map[string]string, error) {
	rs, err := s.c.DB.QueryContext(ctx, `SELECT trust_account_id, qbo_account FROM trust_bank_map`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]string{}
	for rs.Next() {
		var id, v string
		if err := rs.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rs.Err()
}

// Tick is one worker pass: the nightly reconcile when its hour comes, due pushes, the
// reconnect reminder and the failure alert.
func (s *Server) Tick(ctx context.Context) error {
	s.work.Lock()
	defer s.work.Unlock()
	st, err := s.settings(ctx)
	if err != nil {
		return err
	}
	if err := s.reconnectReminder(ctx); err != nil {
		s.log.Printf("reminder: %v", err)
	}
	if due, err := s.nightlyDue(ctx, st); err != nil {
		return err
	} else if due {
		if _, err := s.reconcile(ctx, "nightly", ""); err != nil && !errors.Is(err, errPaused) {
			s.log.Printf("reconcile: %v", err)
		}
	} else if err := s.pushDue(ctx); err != nil && !errors.Is(err, errPaused) {
		s.log.Printf("push: %v", err)
	}
	return s.alertFailures(ctx, st)
}

// pushDue pushes every pending row whose time has come.
func (s *Server) pushDue(ctx context.Context) error {
	var n int
	if err := s.c.DB.QueryRowContext(ctx, dueCount(), s.now().UnixMicro()).Scan(&n); err != nil || n == zero {
		return err
	}
	p, err := s.newPass(ctx, true)
	if err != nil {
		return err
	}
	return p.run(ctx)
}

func dueCount() string {
	var parts []string
	for _, t := range tables {
		parts = append(parts, "(SELECT count(*) FROM "+t.Name+" WHERE (state = 'pending' AND next_attempt_at <= $1) OR (state = 'failed' AND last_error_code = '"+codeMapping+"'))")
	}
	return "SELECT " + strings.Join(parts, " + ")
}

// run pushes the due rows in dependency order: customers, invoices with their
// write-offs, payments, trust entries, expenses, then payment voids and invoice voids.
func (p *pass) run(ctx context.Context) error {
	if err := p.revive(ctx); err != nil {
		return err
	}
	for _, t := range tables {
		rows, err := p.s.store.rows(ctx, t, "state = 'pending' AND next_attempt_at <= $1", p.now.UnixMicro())
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := p.one(ctx, r); err != nil {
				return err
			}
		}
	}
	// A row that waited on one pushed later in this pass (a reversal on its original,
	// a payment on its invoice) gets one more turn now.
	waited := p.waited
	p.waited = nil
	for _, r := range waited {
		if err := p.one(ctx, r); err != nil {
			return err
		}
	}
	for _, j := range p.paymentVoids {
		if err := p.finish(ctx, j.r, p.voidPayment(ctx, j.r)); err != nil {
			return err
		}
	}
	for _, j := range p.invoiceVoids {
		if err := p.finish(ctx, j.r, p.voidInvoice(ctx, j.r, j.inv)); err != nil {
			return err
		}
	}
	return nil
}

// revive puts mapping_invalid rows back in the queue once their mapping is right.
func (p *pass) revive(ctx context.Context) error {
	for _, t := range tables {
		if p.mapErr[t.Kind] != "" {
			continue
		}
		if _, err := p.s.c.DB.ExecContext(ctx, `UPDATE `+t.Name+` SET state = 'pending', attempts = 0, next_attempt_at = $1, updated_at = $1
WHERE state = 'failed' AND last_error_code = $2`, p.now.UnixMicro(), codeMapping); err != nil {
			return err
		}
	}
	return nil
}

// one reads a row's record through the books reader and pushes it.
func (p *pass) one(ctx context.Context, r *Row) error {
	r.LastErrorCode, r.LastError = "", ""
	p.wrote = false
	switch r.T {
	case tCustomer:
		return p.finish(ctx, r, p.pushCustomer(ctx, r))
	case tInvoice:
		inv, err := p.s.core.invoice(ctx, r.Key)
		if err == nil && inv.Status == "void" {
			p.invoiceVoids = append(p.invoiceVoids, voidJob{r: r, inv: inv})
			return nil
		}
		if err == nil {
			err = p.pushInvoice(ctx, r, inv)
		}
		return p.finish(ctx, r, err)
	case tPayment:
		pm, err := p.s.core.payment(ctx, r.Key)
		if err == nil && pm.Status == "reversed" {
			p.paymentVoids = append(p.paymentVoids, voidJob{r: r, p: pm})
			return nil
		}
		if err == nil {
			err = p.pushPayment(ctx, r, pm)
		}
		return p.finish(ctx, r, err)
	case tTrust:
		return p.finish(ctx, r, p.pushTrust(ctx, r))
	case tExpense:
		return p.finish(ctx, r, p.pushExpense(ctx, r))
	}
	return nil
}

// finish records a push's outcome on its row. Only errPaused (and a database error) stops
// the pass.
func (p *pass) finish(ctx context.Context, r *Row, err error) error {
	now := p.now.UnixMicro()
	r.UpdatedAt = now
	var (
		we *waitErr
		se *skipErr
		fe *failErr
	)
	logged := logEntry{}
	switch {
	case err == nil:
		r.State, r.WaitingOn, r.Attempts, r.RequestID = stateSynced, "", zero, ""
		if !p.wrote && r.SkipReason == stateAcknowledged {
			r.State = stateAcknowledged
		}
		r.SkipReason = ""
		if p.wrote || !r.SyncedAt.Valid {
			r.SyncedAt.Int64, r.SyncedAt.Valid = now, true
		}
	case errors.Is(err, errPaused):
		return err
	case errors.As(err, &we):
		r.State, r.WaitingOn, r.NextAttemptAt = statePending, we.on, now
		p.waited = append(p.waited, r)
	case errors.As(err, &se):
		if r.State != stateSkipped || r.SkipReason != se.reason {
			logged = logEntry{Op: "skip", Outcome: "ok", Message: se.reason}
		}
		r.State, r.SkipReason, r.WaitingOn, r.Attempts = stateSkipped, se.reason, "", zero
	case errors.Is(err, errHidden) || isNotFound(err):
		r.State, r.WaitingOn, r.Attempts, r.LastErrorCode, r.LastError = stateHidden, "", zero, "", ""
	case errors.As(err, &fe):
		r.State, r.WaitingOn, r.LastErrorCode, r.LastError = stateFailed, "", fe.code, fe.msg
		logged = logEntry{Op: "skip", Outcome: "failed", FaultCode: fe.code, Message: fe.msg}
	default:
		f := qbo.AsFault(err)
		r.LastErrorCode, r.LastError, r.WaitingOn = f.Code, f.Message, ""
		logged = logEntry{Op: "update", Outcome: "failed", HTTPStatus: f.HTTPStatus, FaultCode: f.Code, Message: f.Message, RequestID: r.RequestID}
		if r.QBOID == "" {
			logged.Op = "create"
		}
		if !f.Retryable {
			r.RequestID = ""
		}
		switch {
		case f.Retryable:
			r.Attempts++
			if r.Attempts >= len(p.st.RetrySchedule) {
				r.State = stateFailed
			} else {
				r.State, r.NextAttemptAt = statePending, p.now.Add(p.st.RetrySchedule[r.Attempts-1]).UnixMicro()
				logged.Outcome = "retry"
			}
		case f.Code == qbo.CodeNotFound:
			r.State, r.LastError = stateDrift, "Deleted in QuickBooks: "+f.Message
			logged.Outcome = "drift"
		case f.Code == qbo.CodeStaleToken:
			r.State, r.LastError = stateDrift, "Changed in QuickBooks by someone else: "+f.Message
			logged.Outcome = "drift"
		default:
			r.State = stateFailed
		}
	}
	if err := p.s.store.save(ctx, r); err != nil {
		return err
	}
	if logged.Op != "" {
		return p.s.store.log(ctx, p.now, p.conn.RealmID, r.T, r.RecordID(), logged)
	}
	return nil
}

func (p *pass) logOK(ctx context.Context, r *Row, op, reqID string) error {
	p.wrote = true
	return p.s.store.log(ctx, p.now, p.conn.RealmID, r.T, r.RecordID(), logEntry{Op: op, Outcome: "ok", RequestID: reqID})
}

// token reads an entity's current SyncToken (after a Re-push, which clears the stored
// one).
func (p *pass) token(ctx context.Context, kind qbo.Kind, id string) (string, error) {
	_, ref, err := p.books.Read(ctx, kind, id)
	return ref.SyncToken, err
}

// put creates or fully updates the row's main entity. It makes no call when the payload
// hash matches what QBO accepted last.
func (p *pass) put(ctx context.Context, r *Row, e qbo.Entity) error {
	h := Hash(e)
	if r.QBOID != "" && r.SyncToken != "" && h == r.PushedHash {
		return nil
	}
	rec := r.RecordID()
	r.RealmID = p.conn.RealmID
	if r.QBOID == "" {
		want := RequestID(rec, "create", h)
		reqID := want
		if r.RequestID != "" {
			reqID = r.RequestID // a retry repeats the call in flight
		} else {
			r.RequestID = reqID
			if err := p.s.store.save(ctx, r); err != nil {
				return err
			}
		}
		ref, err := p.books.Create(ctx, e, reqID)
		if err != nil {
			return err
		}
		r.QBOID, r.SyncToken, r.PushedHash = ref.ID, ref.SyncToken, h
		if reqID != want {
			r.PushedHash = "" // QBO answered the earlier payload; the next pass updates it
			r.NextAttemptAt = p.now.UnixMicro()
		}
		return p.logOK(ctx, r, "create", reqID)
	}
	tok := r.SyncToken
	if tok == "" {
		var err error
		if tok, err = p.token(ctx, e.Kind(), r.QBOID); err != nil {
			return err
		}
	}
	e.SetRef(qbo.Ref{ID: r.QBOID, SyncToken: tok})
	reqID := RequestID(rec, "update:"+tok, h)
	r.RequestID = reqID
	ref, err := p.books.Update(ctx, e, reqID)
	if err != nil {
		return err
	}
	r.SyncToken, r.PushedHash = ref.SyncToken, h
	return p.logOK(ctx, r, "update", reqID)
}

// customerFor returns the QBO customer id for a contact, pushing the customer first if
// it has not synced. A customer that cannot push yet makes the caller wait.
func (p *pass) customerFor(ctx context.Context, contactID string) (string, error) {
	rows, err := p.s.store.customerRows(ctx, contactID)
	if err != nil {
		return "", err
	}
	var r *Row
	for _, x := range rows {
		if x.QBOID != "" {
			r = x
			break
		}
	}
	if r == nil && len(rows) > 0 {
		r = rows[0]
	}
	if r == nil {
		if r, err = p.s.store.newCustomerRow(ctx, contactID, p.now.UnixMicro()); err != nil {
			return "", err
		}
	}
	if r.QBOID != "" && r.State != statePending {
		return r.QBOID, nil
	}
	saved := p.wrote
	p.wrote = false
	err = p.pushCustomer(ctx, r)
	r.LastErrorCode, r.LastError = "", ""
	if ferr := p.finish(ctx, r, err); ferr != nil {
		return "", ferr
	}
	p.wrote = saved
	if r.QBOID == "" || r.State != stateSynced {
		return "", &waitErr{on: "customer"}
	}
	return r.QBOID, nil
}

// pushCustomer creates, adopts or updates the customer for a contact.
func (p *pass) pushCustomer(ctx context.Context, r *Row) error {
	c, err := p.s.core.booksContact(ctx, r.ContactID)
	if err != nil {
		return err
	}
	base := BaseDisplayName(c)
	if base == "" {
		return &failErr{code: "no_name", msg: "The contact has no name to use as the QuickBooks DisplayName"}
	}
	name := base
	if r.DisplayName != "" && strings.HasSuffix(r.DisplayName, SuffixedName("", c.ID)) {
		name = SuffixedName(base, c.ID)
	}
	r.RealmID = p.conn.RealmID
	if r.QBOID == "" {
		existing, err := p.books.CustomerByName(ctx, name)
		if err != nil {
			return err
		}
		if existing != nil {
			if others(MarkerContacts(existing.Notes), c.ID) {
				name = SuffixedName(base, c.ID)
			} else {
				r.QBOID, r.SyncToken, r.Adopted, r.DisplayName = existing.ID, existing.SyncToken, true, name
				if err := p.s.store.save(ctx, r); err != nil {
					return err
				}
				if err := p.logOK(ctx, r, "adopt", ""); err != nil {
					return err
				}
				return p.updateCustomer(ctx, r, BuildCustomer(c, name))
			}
		}
		cu := BuildCustomer(c, name)
		err = p.put(ctx, r, cu)
		if f := qbo.AsFault(err); f != nil && f.Code == qbo.CodeDuplicateName && name == base {
			name = SuffixedName(base, c.ID)
			r.RequestID = ""
			err = p.put(ctx, r, BuildCustomer(c, name))
		}
		if err == nil {
			r.DisplayName = name
		}
		return err
	}
	r.DisplayName = name
	return p.updateCustomer(ctx, r, BuildCustomer(c, name))
}

func others(ids []string, self string) bool {
	for _, id := range ids {
		if id != self {
			return true
		}
	}
	return false
}

// updateCustomer sends a sparse update of the fields the module owns. On a stale
// SyncToken it reads the current one and tries once more: customers are not drift.
func (p *pass) updateCustomer(ctx context.Context, r *Row, cu *qbo.Customer) error {
	h := Hash(cu)
	if h == r.PushedHash && r.SyncToken != "" {
		return nil
	}
	cu.Sparse = true
	send := func(tok string) error {
		cu.SetRef(qbo.Ref{ID: r.QBOID, SyncToken: tok})
		reqID := RequestID(r.ContactID, "update:"+tok, h)
		ref, err := p.books.Update(ctx, cu, reqID)
		if err != nil {
			return err
		}
		r.SyncToken, r.PushedHash = ref.SyncToken, h
		return p.logOK(ctx, r, "update", reqID)
	}
	tok := r.SyncToken
	if tok == "" {
		var err error
		if tok, err = p.token(ctx, qbo.KindCustomer, r.QBOID); err != nil {
			return err
		}
	}
	err := send(tok)
	if f := qbo.AsFault(err); f != nil && f.Code == qbo.CodeStaleToken {
		if tok, err = p.token(ctx, qbo.KindCustomer, r.QBOID); err != nil {
			return err
		}
		return send(tok)
	}
	return err
}

func (p *pass) beforeStart(date string) bool { return p.start != "" && date < p.start }

func (p *pass) startReason() string { return "dated before the sync start date " + p.start }

// pushInvoice posts a sent, viewed or paid invoice and its write-off.
func (p *pass) pushInvoice(ctx context.Context, r *Row, inv Invoice) error {
	switch inv.Status {
	case "sent", "viewed", "paid":
	default:
		return &skipErr{reason: "a " + inv.Status + " invoice never syncs"}
	}
	if p.beforeStart(inv.IssuedOn) {
		return &skipErr{reason: p.startReason()}
	}
	if m := p.mapErr["invoice"]; m != "" {
		return &failErr{code: codeMapping, msg: m}
	}
	m, err := p.s.core.booksMatter(ctx, inv.MatterID)
	if err != nil {
		return err
	}
	cust, err := p.customerFor(ctx, m.ClientContactID)
	if err != nil {
		return err
	}
	q, err := BuildInvoice(inv, m.Number, cust, p.refs)
	var te *ErrTotals
	if errors.As(err, &te) {
		return &failErr{code: "totals_mismatch", msg: te.Error()}
	}
	if err != nil {
		return &failErr{code: codeMapping, msg: err.Error()}
	}
	if rows, _ := p.s.store.customerRows(ctx, m.ClientContactID); len(rows) > 0 {
		r.CustomerSyncID.String, r.CustomerSyncID.Valid = rows[0].Key, true
	}
	if err := p.put(ctx, r, q); err != nil {
		return err
	}
	if err := p.writeOff(ctx, r, inv, cust); err != nil {
		return err
	}
	paid, err := p.paymentsOn(ctx, inv)
	if err != nil {
		return err
	}
	if inv.AmountPaidCents > paid {
		r.LastErrorCode = "paid_without_payments"
		r.LastError = fmt.Sprintf("Togavsu shows %s paid but its payment records total %s; QuickBooks will show a balance of %s",
			qbo.Money(inv.AmountPaidCents), qbo.Money(paid), qbo.Money(inv.AmountPaidCents-paid))
	}
	return nil
}

// paymentsOn totals the completed payments applied to an invoice.
func (p *pass) paymentsOn(ctx context.Context, inv Invoice) (int64, error) {
	list, err := listAll[Payment](ctx, p.s.core, "/api/v1/payments", url.Values{"matter_id": {inv.MatterID}}, "")
	if err != nil {
		return 0, err
	}
	var n int64
	for _, pm := range list {
		if pm.InvoiceID != nil && *pm.InvoiceID == inv.ID && pm.Status == "completed" {
			n += pm.AmountCents
		}
	}
	return n, nil
}

// writeOff posts or raises the credit memo and the zero payment applying it.
func (p *pass) writeOff(ctx context.Context, r *Row, inv Invoice, cust string) error {
	if inv.WriteOffCents <= zero64 || inv.WriteOffCents == r.WriteOffCents && r.WriteOffLinkID != "" {
		return nil
	}
	date := p.localDate(p.now)
	if date < inv.IssuedOn {
		date = inv.IssuedOn
	}
	cm := BuildCreditMemo(inv, cust, date, p.refs)
	h := Hash(cm)
	if r.CreditMemoID == "" {
		reqID := RequestID(inv.ID, "writeoff-create", h)
		ref, err := p.books.Create(ctx, cm, reqID)
		if err != nil {
			return err
		}
		r.CreditMemoID, r.CreditMemoTok = ref.ID, ref.SyncToken
		if err := p.saveLog(ctx, r, "create", reqID); err != nil {
			return err
		}
	} else {
		cm.SetRef(qbo.Ref{ID: r.CreditMemoID, SyncToken: r.CreditMemoTok})
		reqID := RequestID(inv.ID, "writeoff-update:"+r.CreditMemoTok, h)
		ref, err := p.books.Update(ctx, cm, reqID)
		if err != nil {
			return err
		}
		r.CreditMemoTok = ref.SyncToken
		if err := p.saveLog(ctx, r, "update", reqID); err != nil {
			return err
		}
	}
	link := BuildWriteOffLink(inv, cust, date, r.QBOID, r.CreditMemoID)
	lh := Hash(link)
	if r.WriteOffLinkID == "" {
		reqID := RequestID(inv.ID, "writeoff-link", lh)
		ref, err := p.books.Create(ctx, link, reqID)
		if err != nil {
			return err
		}
		r.WriteOffLinkID = ref.ID
		if err := p.saveLog(ctx, r, "link", reqID); err != nil {
			return err
		}
	} else {
		tok, err := p.token(ctx, qbo.KindPayment, r.WriteOffLinkID)
		if err != nil {
			return err
		}
		link.SetRef(qbo.Ref{ID: r.WriteOffLinkID, SyncToken: tok})
		reqID := RequestID(inv.ID, "writeoff-link-update:"+tok, lh)
		if _, err := p.books.Update(ctx, link, reqID); err != nil {
			return err
		}
		if err := p.saveLog(ctx, r, "link", reqID); err != nil {
			return err
		}
	}
	r.WriteOffCents = inv.WriteOffCents
	return nil
}

// saveLog stores a row after a secondary write so a later failure never repeats it.
func (p *pass) saveLog(ctx context.Context, r *Row, op, reqID string) error {
	if err := p.s.store.save(ctx, r); err != nil {
		return err
	}
	return p.logOK(ctx, r, op, reqID)
}

// voidInvoice deletes the write-off link and credit memo, then voids the invoice, once
// every synced payment on it is voided.
func (p *pass) voidInvoice(ctx context.Context, r *Row, inv Invoice) error {
	if r.QBOID == "" {
		return &skipErr{reason: "voided before it synced"}
	}
	if r.Voided {
		return nil
	}
	list, err := listAll[Payment](ctx, p.s.core, "/api/v1/payments", url.Values{"matter_id": {inv.MatterID}}, "")
	if err != nil {
		return err
	}
	for _, pm := range list {
		if pm.InvoiceID == nil || *pm.InvoiceID != inv.ID {
			continue
		}
		pr, err := p.s.store.row(ctx, tPayment, pm.ID)
		if err != nil {
			return err
		}
		if pr != nil && pr.QBOID != "" && !pr.Voided {
			return &waitErr{on: "payment void"}
		}
	}
	if r.WriteOffLinkID != "" {
		if err := p.remove(ctx, r, qbo.KindPayment, r.WriteOffLinkID); err != nil {
			return err
		}
		r.WriteOffLinkID = ""
		if err := p.s.store.save(ctx, r); err != nil {
			return err
		}
	}
	if r.CreditMemoID != "" {
		if err := p.remove(ctx, r, qbo.KindCreditMemo, r.CreditMemoID); err != nil {
			return err
		}
		r.CreditMemoID, r.CreditMemoTok, r.WriteOffCents = "", "", zero64
		if err := p.s.store.save(ctx, r); err != nil {
			return err
		}
	}
	return p.void(ctx, r, qbo.KindInvoice)
}

func (p *pass) remove(ctx context.Context, r *Row, kind qbo.Kind, id string) error {
	tok, err := p.token(ctx, kind, id)
	if err != nil {
		return err
	}
	reqID := RequestID(r.Key, "delete:"+id, tok)
	if err := p.books.Delete(ctx, kind, qbo.Ref{ID: id, SyncToken: tok}, reqID); err != nil {
		return err
	}
	return p.logOK(ctx, r, "delete", reqID)
}

func (p *pass) void(ctx context.Context, r *Row, kind qbo.Kind) error {
	tok := r.SyncToken
	if tok == "" {
		var err error
		if tok, err = p.token(ctx, kind, r.QBOID); err != nil {
			return err
		}
	}
	reqID := RequestID(r.Key, "void", tok)
	ref, err := p.books.Void(ctx, kind, qbo.Ref{ID: r.QBOID, SyncToken: tok}, reqID)
	if err != nil {
		return err
	}
	r.SyncToken, r.Voided = ref.SyncToken, true
	return p.logOK(ctx, r, "void", reqID)
}

// pushPayment posts a completed operating payment.
func (p *pass) pushPayment(ctx context.Context, r *Row, pm Payment) error {
	if pm.TrustEntryID != nil {
		r.TrustEntryID.String, r.TrustEntryID.Valid = *pm.TrustEntryID, true
	}
	if pm.Destination == "trust" {
		return &skipErr{reason: "posted from trust ledger entry"}
	}
	if pm.Status != "completed" {
		return &skipErr{reason: "a " + pm.Status + " payment never syncs"}
	}
	if pm.AmountCents <= zero64 {
		return &skipErr{reason: "zero amount"}
	}
	date := p.localDate(pm.CreatedAt)
	if p.beforeStart(date) {
		return &skipErr{reason: p.startReason()}
	}
	if m := p.mapErr["payment"]; m != "" {
		return &failErr{code: codeMapping, msg: m}
	}
	invoiceQBO := ""
	if pm.InvoiceID != nil {
		ir, err := p.s.store.row(ctx, tInvoice, *pm.InvoiceID)
		if err != nil {
			return err
		}
		switch {
		case ir != nil && ir.QBOID != "":
			invoiceQBO = ir.QBOID
		case ir != nil && ir.State == stateSkipped && strings.HasPrefix(ir.SkipReason, "dated before"):
			// applied to an invoice before the start date: posts unapplied
		default:
			inv, err := p.s.core.invoice(ctx, *pm.InvoiceID)
			if err != nil {
				return err
			}
			if !p.beforeStart(inv.IssuedOn) {
				if ir == nil {
					if err := p.s.store.queue(ctx, tInvoice, inv.ID, p.now.UnixMicro()); err != nil {
						return err
					}
				}
				return &waitErr{on: "invoice"}
			}
		}
	}
	cust, err := p.customerFor(ctx, pm.ContactID)
	if err != nil {
		return err
	}
	mode := p.st.PaymentDeposit
	if pm.TrustEntryID != nil {
		mode = "operating_bank" // the operating side of a trust transfer
	}
	return p.put(ctx, r, BuildPayment(pm, cust, date, invoiceQBO, mode, p.refs))
}

// voidPayment voids a reversed payment's QBO Payment.
func (p *pass) voidPayment(ctx context.Context, r *Row) error {
	if r.QBOID == "" {
		return &skipErr{reason: "reversed before it synced"}
	}
	if r.Voided {
		return nil
	}
	return p.void(ctx, r, qbo.KindPayment)
}

// trustBank is the QBO bank for a Togavsu trust account.
func (p *pass) trustBank(accountID string) qbo.Account {
	if a, ok := p.refs.TrustBankFor[accountID]; ok {
		return a
	}
	return p.refs.TrustBank
}

// linkedPayment finds the payment a transfer to operating paid an invoice with.
func (p *pass) linkedPayment(ctx context.Context, e TrustEntry) (*Payment, error) {
	list, err := listAll[Payment](ctx, p.s.core, "/api/v1/payments", url.Values{"matter_id": {e.MatterID}}, "")
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].TrustEntryID != nil && *list[i].TrustEntryID == e.ID {
			return &list[i], nil
		}
	}
	return nil, nil
}

// trustLines builds an entry's journal lines; a reversal mirrors the entry it reverses.
func (p *pass) trustLines(ctx context.Context, r *Row, e TrustEntry, cust string) ([]qbo.Line, string, error) {
	bank := p.trustBank(e.TrustAccountID)
	if e.EntryType == "reversal" {
		if e.ReversesEntryID == nil {
			return nil, "", &failErr{code: "no_original", msg: "The reversal names no entry"}
		}
		orow, err := p.s.store.row(ctx, tTrust, *e.ReversesEntryID)
		if err != nil {
			return nil, "", err
		}
		if orow == nil || orow.QBOID == "" {
			if orow == nil {
				if err := p.s.store.queue(ctx, tTrust, *e.ReversesEntryID, p.now.UnixMicro()); err != nil {
					return nil, "", err
				}
			}
			return nil, "", &waitErr{on: "reversed entry"}
		}
		orig, err := p.s.core.trustEntry(ctx, *e.ReversesEntryID)
		if err != nil {
			return nil, "", err
		}
		lines, _ := TrustLines(orig, cust, p.trustBank(orig.TrustAccountID), orow.Shape == shapeTwo && orig.EntryType == "transfer_to_operating", p.refs)
		for i := range lines {
			lines[i].Description = e.Description
		}
		return Mirror(lines), shapeMirror, nil
	}
	linked := false
	if e.EntryType == "transfer_to_operating" {
		lp, err := p.linkedPayment(ctx, e)
		if err != nil {
			return nil, "", err
		}
		if lp != nil {
			linked = true
			r.LinkedPaymentID.String, r.LinkedPaymentID.Valid = lp.ID, true
		}
	}
	lines, shape := TrustLines(e, cust, bank, linked, p.refs)
	if lines == nil {
		return nil, "", &skipErr{reason: "entry type " + e.EntryType + " has no QuickBooks posting"}
	}
	return lines, shape, nil
}

// pushTrust posts a trust ledger entry as a JournalEntry.
func (p *pass) pushTrust(ctx context.Context, r *Row) error {
	e, err := p.s.core.trustEntry(ctx, r.Key)
	if err != nil {
		return err
	}
	if e.AmountCents == zero64 {
		return &skipErr{reason: "zero amount"}
	}
	date := p.localDate(e.CreatedAt)
	if p.beforeStart(date) {
		return &skipErr{reason: p.startReason()}
	}
	if m := p.mapErr["trust_entry"]; m != "" {
		return &failErr{code: codeMapping, msg: m}
	}
	if _, err := p.s.core.booksMatter(ctx, e.MatterID); err != nil {
		return err
	}
	cust, err := p.customerFor(ctx, e.ClientID)
	if err != nil {
		return err
	}
	lines, shape, err := p.trustLines(ctx, r, e, cust)
	if err != nil {
		return err
	}
	r.Shape = shape
	return p.put(ctx, r, BuildJournalEntry(e, date, lines))
}

// pushExpense posts an expense as a Purchase in purchases mode.
func (p *pass) pushExpense(ctx context.Context, r *Row) error {
	if p.st.ExpensePush != "purchases" {
		return &skipErr{reason: "expense_push is invoice_lines_only; the expense reaches QuickBooks as an invoice line"}
	}
	x, err := p.s.core.expense(ctx, r.Key)
	if err != nil {
		return err
	}
	if p.beforeStart(x.IncurredOn) {
		return &skipErr{reason: p.startReason()}
	}
	if m := p.mapErr["expense"]; m != "" {
		return &failErr{code: codeMapping, msg: m}
	}
	m, err := p.s.core.booksMatter(ctx, x.MatterID)
	if err != nil {
		return err
	}
	cust, err := p.customerFor(ctx, m.ClientContactID)
	if err != nil {
		return err
	}
	return p.put(ctx, r, BuildPurchase(x, cust, p.refs))
}
