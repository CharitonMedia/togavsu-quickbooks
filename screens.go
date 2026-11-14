package quickbooks

import (
	"context"
	"embed"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
)

//go:embed templates
var templateFS embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"money": func(c int64) string { return qbo.Money(c).String() },
	"when": func(us int64) string {
		if us == 0 {
			return ""
		}
		return time.UnixMicro(us).UTC().Format("2006-01-02 15:04 UTC")
	},
}).ParseFS(templateFS, "templates/*.html"))

// dataFunc builds one screen's data for a verified viewer. The same data serves the
// HTML page and its JSON twin under /api/.
type dataFunc func(ctx context.Context, v Viewer, r *http.Request) (any, error)

// Base is on every screen.
type Base struct {
	Sandbox     bool   `json:"sandbox"`
	Environment string `json:"environment"`
	Context     string `json:"-"`
	Done        string `json:"-"`
	Note        string `json:"note,omitempty"`
}

func (s *Server) base(ctx context.Context, v Viewer, r *http.Request) Base {
	env := "sandbox"
	if st, err := s.settings(ctx); err == nil {
		env = st.Environment
	}
	return Base{Sandbox: env == "sandbox", Environment: env, Context: v.Token, Done: r.URL.Query().Get("done")}
}

func (s *Server) page(f dataFunc, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := s.viewer(r)
		if err != nil {
			http.Error(w, "open this page through Togavsu", http.StatusUnauthorized)
			return
		}
		data, err := f(r.Context(), v, r)
		if err != nil {
			status, msg := statusOf(err)
			http.Error(w, msg, status)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
		if err := pages.ExecuteTemplate(w, name+".html", data); err != nil {
			s.log.Printf("render %s: %v", name, err)
		}
	}
}

func (s *Server) api(f dataFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := s.viewer(r)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "a context token is required"})
			return
		}
		data, err := f(r.Context(), v, r)
		if err != nil {
			status, msg := statusOf(err)
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		writeJSON(w, http.StatusOK, data)
	}
}

var (
	errNotFound = errStatus(http.StatusNotFound, "not found")
	moneyRoles  = []string{"admin", "accountant"}
)

// Counts are the widget's numbers. SyncedToday counts rows that reached QuickBooks since
// midnight in the firm's time zone.
type Counts struct {
	SyncedToday int `json:"synced_today"`
	Synced      int `json:"synced"`
	Pending     int `json:"pending"`
	Exceptions  int `json:"exceptions"`
}

// visibleRows reads the module's rows and keeps those whose records the books reader
// returns; the rest move to hidden. With q set the books reader lists only that slice
// (one matter).
func (s *Server) visibleRows(ctx context.Context, q url.Values, withCustomers bool) ([]*Row, *seen, *pass, error) {
	s.work.Lock() // it may move rows to hidden
	defer s.work.Unlock()
	p, err := s.newPass(ctx, false)
	if err != nil {
		return nil, nil, nil, err
	}
	sn, err := s.fullRead(ctx, p, q)
	if err != nil {
		return nil, nil, nil, err
	}
	now := s.now().UnixMicro()
	var out []*Row
	for _, t := range []*table{tInvoice, tPayment, tTrust, tExpense} {
		rows, err := s.store.rows(ctx, t, "state <> 'hidden'")
		if err != nil {
			return nil, nil, nil, err
		}
		if q != nil {
			var mine []*Row
			for _, r := range rows {
				if sn.all[t][r.Key] {
					mine = append(mine, r)
				}
			}
			out = append(out, mine...)
			continue
		}
		kept, err := s.confirm(ctx, sn, t, rows, now)
		if err != nil {
			return nil, nil, nil, err
		}
		out = append(out, kept...)
	}
	if withCustomers {
		rows, err := s.store.rows(ctx, tCustomer, "state <> 'hidden'")
		if err != nil {
			return nil, nil, nil, err
		}
		kept, err := s.confirmCustomers(ctx, rows, now, false)
		if err != nil {
			return nil, nil, nil, err
		}
		out = append(out, kept...)
	}
	return out, sn, p, nil
}

func isException(r *Row) bool {
	return r.State == stateFailed || r.State == stateDrift || (r.State == stateSynced && r.LastErrorCode == "paid_without_payments")
}

func count(rows []*Row, dayStart int64) Counts {
	var c Counts
	for _, r := range rows {
		switch {
		case isException(r):
			c.Exceptions++
		case r.State == stateSynced || r.State == stateAcknowledged:
			if r.T != tCustomer {
				c.Synced++
				if r.SyncedAt.Valid && r.SyncedAt.Int64 >= dayStart {
					c.SyncedToday++
				}
			}
		case r.State == statePending:
			c.Pending++
		}
	}
	return c
}

// WidgetView is the sync-status widget.
type WidgetView struct {
	Base
	Connection string            `json:"connection"`
	Company    string            `json:"company"`
	Counts     Counts            `json:"counts"`
	Links      map[string]string `json:"links"`
}

func (s *Server) widgetData(ctx context.Context, v Viewer, r *http.Request) (any, error) {
	if !v.Has(moneyRoles...) {
		return nil, errNotFound
	}
	rows, _, p, err := s.visibleRows(ctx, nil, true)
	if err != nil {
		return nil, err
	}
	conn, err := s.store.connection(ctx)
	if err != nil {
		return nil, err
	}
	tab := "/modules/" + ID + "/tabs/books?status="
	return WidgetView{Base: s.base(ctx, v, r), Connection: conn.State, Company: conn.CompanyName, Counts: count(rows, p.dayStart()),
		Links: map[string]string{"synced": tab + "synced", "pending": tab + "pending", "exceptions": tab + "exceptions"}}, nil
}

// RowView is one sync row on a screen.
type RowView struct {
	Kind      string `json:"kind"`
	RecordID  string `json:"record_id"`
	State     string `json:"state"`
	Reason    string `json:"skip_reason,omitempty"`
	WaitingOn string `json:"waiting_on,omitempty"`
	QBOID     string `json:"qbo_id,omitempty"`
	Code      string `json:"error_code,omitempty"`
	Error     string `json:"error,omitempty"`
	Attempts  int    `json:"attempts"`
	CanRetry  bool   `json:"can_retry"`
	CanDrift  bool   `json:"can_repush_or_acknowledge"`
}

func rowView(r *Row) RowView {
	return RowView{Kind: r.T.Kind, RecordID: r.RecordID(), State: r.State, Reason: r.SkipReason, WaitingOn: r.WaitingOn, QBOID: r.QBOID,
		Code: r.LastErrorCode, Error: r.LastError, Attempts: r.Attempts, CanRetry: r.State == stateFailed, CanDrift: r.State == stateDrift}
}

// BookLine is one record in the books view.
type BookLine struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Date        string `json:"date"`
	Label       string `json:"label"`
	AmountCents int64  `json:"amount_cents"`
	State       string `json:"qbo_state"`
	QBOID       string `json:"qbo_id,omitempty"`
}

// BooksView is the books tab.
type BooksView struct {
	Base
	CanAct     bool       `json:"can_act"`
	Connection string     `json:"connection"`
	Company    string     `json:"company"`
	LastRun    int64      `json:"last_run_at"`
	Counts     Counts     `json:"counts"`
	Status     string     `json:"status"`
	Rows       []RowView  `json:"rows"`
	Month      string     `json:"month"`
	Lines      []BookLine `json:"lines"`
	Togavsu    int64      `json:"togavsu_total_cents"`
	InQBO      int64      `json:"qbo_total_cents"`
}

func (s *Server) booksData(ctx context.Context, v Viewer, r *http.Request) (any, error) {
	b := s.base(ctx, v, r)
	if !v.Has(moneyRoles...) {
		b.Note = "The QuickBooks books view is for admins and accountants."
		return BooksView{Base: b}, nil
	}
	rows, sn, p, err := s.visibleRows(ctx, nil, true)
	if err != nil {
		return nil, err
	}
	conn, err := s.store.connection(ctx)
	if err != nil {
		return nil, err
	}
	out := BooksView{Base: b, CanAct: true, Connection: conn.State, Company: conn.CompanyName, Counts: count(rows, p.dayStart()), Status: r.URL.Query().Get("status")}
	if out.Status == "" {
		out.Status = "exceptions"
	}
	_ = s.c.DB.QueryRowContext(ctx, `SELECT coalesce(max(finished_at), 0) FROM runs`).Scan(&out.LastRun)
	for _, row := range rows {
		ok := false
		switch out.Status {
		case "exceptions":
			ok = isException(row)
		case "synced":
			ok = !isException(row) && (row.State == stateSynced || row.State == stateAcknowledged)
		default:
			ok = row.State == out.Status
		}
		if ok {
			out.Rows = append(out.Rows, rowView(row))
		}
	}
	out.Month = r.URL.Query().Get("month")
	if _, err := time.Parse("2006-01", out.Month); err != nil {
		out.Month = p.localDate(p.now)[:7]
	}
	out.Lines, out.Togavsu, out.InQBO, err = s.bookLines(ctx, v, p, sn, rows, nil, func(date string) bool { return strings.HasPrefix(date, out.Month) })
	return out, err
}

// bookLines lists the records the viewer's own token returns, kept only when the books
// reader returns them too, joined to the module's rows.
func (s *Server) bookLines(ctx context.Context, v Viewer, p *pass, sn *seen, rows []*Row, q url.Values, keep func(string) bool) ([]BookLine, int64, int64, error) {
	byKey := map[string]*Row{}
	for _, r := range rows {
		byKey[r.T.Kind+"/"+r.Key] = r
	}
	var lines []BookLine
	add := func(t *table, id, date, label string, cents int64) {
		if !sn.all[t][id] || !keep(date) {
			return
		}
		l := BookLine{Kind: t.Kind, ID: id, Date: date, Label: label, AmountCents: cents, State: "not synced"}
		if r := byKey[t.Kind+"/"+id]; r != nil {
			l.State, l.QBOID = r.State, r.QBOID
		}
		lines = append(lines, l)
	}
	vals := func(extra ...string) url.Values {
		out := url.Values{}
		for k, x := range q {
			out[k] = x
		}
		if len(extra) == 2 {
			out.Set(extra[0], extra[1])
		}
		return out
	}
	invs, err := listAll[Invoice](ctx, s.core, "/api/v1/invoices", vals("include", ID+".invoice_sync"), v.Token)
	if err != nil {
		return nil, 0, 0, err
	}
	for _, x := range invs {
		if x.Status != "draft" {
			add(tInvoice, x.ID, x.IssuedOn, "Invoice "+x.Number, x.TotalCents)
		}
	}
	pays, err := listAll[Payment](ctx, s.core, "/api/v1/payments", vals("include", ID+".payment_sync"), v.Token)
	if err != nil {
		return nil, 0, 0, err
	}
	for _, x := range pays {
		add(tPayment, x.ID, p.localDate(x.CreatedAt), "Payment "+x.Method+" "+x.Status, x.AmountCents)
	}
	ents, err := listAll[TrustEntry](ctx, s.core, "/api/v1/trust-ledger", vals(), v.Token)
	if err != nil {
		return nil, 0, 0, err
	}
	for _, x := range ents {
		add(tTrust, x.ID, p.localDate(x.CreatedAt), "Trust "+strings.ReplaceAll(x.EntryType, "_", " "), x.Signed())
	}
	exps, err := listAll[Expense](ctx, s.core, "/api/v1/expenses", vals(), v.Token)
	if err != nil {
		return nil, 0, 0, err
	}
	for _, x := range exps {
		add(tExpense, x.ID, x.IncurredOn, "Expense "+x.Description, x.AmountCents)
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Date < lines[j].Date })
	var tog, inQBO int64
	for _, l := range lines {
		tog += l.AmountCents
		if l.QBOID != "" && (l.State == stateSynced || l.State == stateAcknowledged) {
			inQBO += l.AmountCents
		}
	}
	return lines, tog, inQBO, nil
}

// SetupView is the setup tab.
type SetupView struct {
	Base
	CanAct       bool           `json:"can_act"`
	RedirectURI  string         `json:"redirect_uri"`
	ConnectURL   string         `json:"connect_url,omitempty"`
	ClientID     bool           `json:"client_id_set"`
	SecretSet    bool           `json:"client_secret_set"`
	Connection   Connection     `json:"connection"`
	Checks       []MappingCheck `json:"mapping"`
	Accounts     []qbo.Account  `json:"accounts"`
	Items        []qbo.Item     `json:"items"`
	TrustBanks   []TrustBankRow `json:"trust_bank_map"`
	ChartProblem string         `json:"chart_problem,omitempty"`
	Missing      []string       `json:"missing_defaults"`
}

// TrustBankRow is one Togavsu trust account and the QBO bank it posts to.
type TrustBankRow struct {
	TrustAccountID string `json:"trust_account_id"`
	Name           string `json:"name"`
	QBOAccount     string `json:"qbo_account"`
}

func (s *Server) setupData(ctx context.Context, v Viewer, r *http.Request) (any, error) {
	if !v.Has(moneyRoles...) {
		return nil, errNotFound
	}
	st, err := s.settings(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := s.store.connection(ctx)
	if err != nil {
		return nil, err
	}
	_, secretSet, err := s.core.getSecret(ctx, secretClient)
	if err != nil {
		return nil, err
	}
	out := SetupView{Base: s.base(ctx, v, r), CanAct: v.Has("admin"), RedirectURI: redirectURI(r), ClientID: st.ClientID != "",
		SecretSet: secretSet, Connection: conn}
	if out.CanAct && out.ClientID && secretSet {
		n, err := s.newNonce(ctx, v.UserID, out.RedirectURI)
		if err != nil {
			return nil, err
		}
		out.ConnectURL = "oauth/start?nonce=" + n
	}
	accts, err := listAll[TrustAccount](ctx, s.core, "/api/v1/trust-accounts", nil, "")
	if err != nil {
		return nil, err
	}
	bm, err := s.trustBankMap(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range accts {
		out.TrustBanks = append(out.TrustBanks, TrustBankRow{TrustAccountID: a.ID, Name: a.Name, QBOAccount: bm[a.ID]})
	}
	if conn.State == "connected" {
		b := s.c.QBO.Open(qbo.Conn{Environment: conn.Environment, RealmID: conn.RealmID}, &tokenSource{s: s, st: st})
		if out.Accounts, err = b.Accounts(ctx); err == nil {
			out.Items, err = b.Items(ctx)
		}
		if err != nil {
			out.ChartProblem = "QuickBooks did not return the chart of accounts: " + qbo.AsFault(err).Message
		} else {
			out.Checks = checkMapping(st, out.Accounts, out.Items)
			out.Missing = missingDefaults(out.Accounts, out.Items)
		}
	}
	return out, nil
}

// MatterView is the matter tab.
type MatterView struct {
	Base
	Matter Matter     `json:"matter"`
	Lines  []BookLine `json:"lines"`
}

func (s *Server) matterData(ctx context.Context, v Viewer, r *http.Request) (any, error) {
	if !v.Has("admin", "attorney", "staff") {
		return nil, errNotFound
	}
	id := r.URL.Query().Get("matter_id")
	if v.MatterID != nil {
		id = *v.MatterID
	}
	if id == "" {
		return nil, errStatus(http.StatusBadRequest, "open this tab from a matter")
	}
	var m Matter
	if err := s.core.do(ctx, http.MethodGet, "/api/v1/matters/"+url.PathEscape(id), v.Token, nil, &m); err != nil {
		return nil, err
	}
	q := url.Values{"matter_id": {id}}
	rows, sn, p, err := s.visibleRows(ctx, q, false)
	if err != nil {
		if isNotFound(err) {
			return nil, errNotFound
		}
		return nil, err
	}
	lines, _, _, err := s.bookLines(ctx, v, p, sn, rows, q, func(string) bool { return true })
	return MatterView{Base: s.base(ctx, v, r), Matter: m, Lines: lines}, err
}

// The defaults Create missing makes (section 3 of the spec). It never makes a bank.
var (
	defaultAccounts = []qbo.Account{
		{Name: "Client Trust Liability", AccountType: "Other Current Liability", AccountSubType: "TrustAccountsLiabilities", Active: true},
		{Name: "Legal Fee Income", AccountType: "Income", AccountSubType: "ServiceFeeIncome", Active: true},
		{Name: "Cost Recovery Income", AccountType: "Income", AccountSubType: "OtherPrimaryIncome", Active: true},
		{Name: "Client Costs Advanced", AccountType: "Other Current Asset", AccountSubType: "OtherCurrentAssets", Active: true},
	}
	defaultItems = []struct{ name, income string }{
		{"Legal Services", "Legal Fee Income"}, {"Flat Fee", "Legal Fee Income"}, {"Reimbursed Costs", "Cost Recovery Income"},
		{"Invoice Adjustment", "Legal Fee Income"}, {"Late Interest", "Legal Fee Income"}, {"Write-Off", "Legal Fee Income"},
	}
)

func missingDefaults(accounts []qbo.Account, items []qbo.Item) []string {
	var out []string
	for _, a := range defaultAccounts {
		if _, ok := ResolveAccount(a.Name, accounts); !ok {
			out = append(out, "account "+a.Name)
		}
	}
	for _, i := range defaultItems {
		if _, ok := ResolveItem(i.name, items); !ok {
			out = append(out, "item "+i.name)
		}
	}
	return out
}

// createMissing makes the default accounts and items that the company lacks.
func (s *Server) createMissing(ctx context.Context) (string, error) {
	p, err := s.newPass(ctx, false)
	if err != nil {
		return "", err
	}
	if p.conn.State != "connected" {
		return "", errStatus(http.StatusConflict, "Connect QuickBooks first")
	}
	b := s.c.QBO.Open(qbo.Conn{Environment: p.conn.Environment, RealmID: p.conn.RealmID}, &tokenSource{s: s, st: p.st})
	accounts, err := b.Accounts(ctx)
	if err != nil {
		return "", err
	}
	var made []string
	for _, a := range defaultAccounts {
		if _, ok := ResolveAccount(a.Name, accounts); ok {
			continue
		}
		ref, err := b.CreateAccount(ctx, a, RequestID(p.conn.RealmID, "account", a.Name))
		if err != nil {
			return strings.Join(made, ", "), err
		}
		a.ID = ref.ID
		accounts = append(accounts, a)
		made = append(made, a.Name)
	}
	items, err := b.Items(ctx)
	if err != nil {
		return "", err
	}
	for _, d := range defaultItems {
		if _, ok := ResolveItem(d.name, items); ok {
			continue
		}
		inc, _ := ResolveAccount(d.income, accounts)
		it := qbo.Item{Name: d.name, Type: "Service", IncomeAccountRef: &qbo.RefVal{Value: inc.ID, Name: inc.Name}, Active: true}
		if _, err := b.CreateItem(ctx, it, RequestID(p.conn.RealmID, "item", d.name)); err != nil {
			return strings.Join(made, ", "), err
		}
		made = append(made, d.name)
	}
	if len(made) == 0 {
		return "Every default account and item already exists.", nil
	}
	return "Created " + strings.Join(made, ", ") + ".", nil
}

// dayStart is midnight today in the firm's time zone, in microseconds.
func (p *pass) dayStart() int64 {
	l := p.now.In(p.tz)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, p.tz).UnixMicro()
}
