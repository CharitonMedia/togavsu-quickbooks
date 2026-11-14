package quickbooks

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// action is one thing a person or agent can do. Each one is a form post from the frame
// and a JSON endpoint under /api/, both verified by the context token.
type action struct {
	path    string
	summary string
	tab     string
	roles   []string
	params  []string
	run     func(s *Server, ctx context.Context, v Viewer, f url.Values) (string, error)
}

var actions = []action{
	{"retry", "Retry a failed row now", "books", moneyRoles, []string{"kind", "id"}, (*Server).actRetry},
	{"repush", "Re-push Togavsu's version over a drift row", "books", moneyRoles, []string{"kind", "id"}, (*Server).actRepush},
	{"acknowledge", "Keep a drift row quiet until the Togavsu record changes", "books", moneyRoles, []string{"kind", "id"}, (*Server).actAcknowledge},
	{"sync-now", "Run the reconcile now", "books", moneyRoles, nil, (*Server).actSyncNow},
	{"disconnect", "Disconnect QuickBooks (revokes the sign-in; sync rows stay)", "setup", []string{"admin"}, nil, (*Server).actDisconnect},
	{"create-missing", "Create the default accounts and items QuickBooks lacks (never a bank)", "setup", []string{"admin"}, nil, (*Server).actCreateMissing},
	{"trust-bank-map", "Map a Togavsu trust account to a QuickBooks bank (empty clears it)", "setup", []string{"admin"}, []string{"trust_account_id", "qbo_account"}, (*Server).actTrustBankMap},
	{"client-secret", "Store the Intuit app's client secret in the core secret store", "setup", []string{"admin"}, []string{"client_secret"}, (*Server).actClientSecret},
	{"reset", "Clear the five sync tables (keeps the sync log); needs confirm=RESET", "setup", []string{"admin"}, []string{"confirm"}, (*Server).actReset},
}

// actionRow finds the row a person names. Only rows whose record the books reader
// returns can be acted on.
func (s *Server) actionRow(ctx context.Context, f url.Values) (*Row, error) {
	t := tableFor(f.Get("kind"))
	if t == nil || f.Get("id") == "" {
		return nil, errStatus(http.StatusBadRequest, "kind and id name the row")
	}
	var r *Row
	var err error
	if t == tCustomer {
		rows, e := s.store.customerRows(ctx, f.Get("id"))
		if len(rows) > 0 {
			r = rows[0]
		}
		err = e
	} else {
		r, err = s.store.row(ctx, t, f.Get("id"))
	}
	if err != nil {
		return nil, err
	}
	if r == nil || r.State == stateHidden {
		return nil, errNotFound
	}
	ok, err := s.visibleToReader(ctx, r)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errNotFound
	}
	return r, nil
}

func (s *Server) logAction(ctx context.Context, r *Row, op string, v Viewer) error {
	return s.store.log(ctx, s.now(), r.RealmID, r.T, r.RecordID(), logEntry{Op: op, Outcome: "ok", Actor: v.UserID})
}

func (s *Server) actRetry(ctx context.Context, v Viewer, f url.Values) (string, error) {
	r, err := s.actionRow(ctx, f)
	if err != nil {
		return "", err
	}
	if r.State != stateFailed {
		return "", errStatus(http.StatusConflict, "only a failed row can be retried")
	}
	now := s.now().UnixMicro()
	r.State, r.Attempts, r.NextAttemptAt, r.UpdatedAt = statePending, zero, now, now
	if err := s.store.save(ctx, r); err != nil {
		return "", err
	}
	if err := s.logAction(ctx, r, "retry_requested", v); err != nil {
		return "", err
	}
	if err := s.pushDue(ctx); err != nil && !errors.Is(err, errPaused) {
		return "Queued; the push will try again.", nil
	}
	return "Retried.", nil
}

func (s *Server) actRepush(ctx context.Context, v Viewer, f url.Values) (string, error) {
	r, err := s.actionRow(ctx, f)
	if err != nil {
		return "", err
	}
	if r.State != stateDrift {
		return "", errStatus(http.StatusConflict, "only a drift row can be re-pushed")
	}
	now := s.now().UnixMicro()
	// An empty SyncToken makes the push read the current one and send Togavsu's version.
	r.State, r.SyncToken, r.PushedHash, r.CreditMemoTok, r.Attempts, r.NextAttemptAt, r.UpdatedAt = statePending, "", "", "", zero, now, now
	if r.LastErrorCode == "drift" && strings.Contains(r.LastError, "deleted") {
		r.QBOID = "" // deleted in QBO: create it again
	}
	if err := s.store.save(ctx, r); err != nil {
		return "", err
	}
	if err := s.logAction(ctx, r, "retry_requested", v); err != nil {
		return "", err
	}
	if err := s.pushDue(ctx); err != nil && !errors.Is(err, errPaused) {
		return "Queued; the push will try again.", nil
	}
	return "Re-pushed.", nil
}

func (s *Server) actAcknowledge(ctx context.Context, v Viewer, f url.Values) (string, error) {
	r, err := s.actionRow(ctx, f)
	if err != nil {
		return "", err
	}
	if r.State != stateDrift {
		return "", errStatus(http.StatusConflict, "only a drift row can be acknowledged")
	}
	r.State, r.UpdatedAt = stateAcknowledged, s.now().UnixMicro()
	if err := s.store.save(ctx, r); err != nil {
		return "", err
	}
	return "Acknowledged.", s.logAction(ctx, r, "acknowledge", v)
}

func (s *Server) actSyncNow(ctx context.Context, v Viewer, _ url.Values) (string, error) {
	run, err := s.reconcile(ctx, "manual", v.UserID)
	if errors.Is(err, errPaused) {
		return "Read the records; pushes wait until QuickBooks is connected.", nil
	}
	if err != nil {
		return "", err
	}
	return "Reconciled " + itoa(run.Examined) + " records.", nil
}

func (s *Server) actDisconnect(ctx context.Context, _ Viewer, _ url.Values) (string, error) {
	return "Disconnected.", s.disconnect(ctx)
}

func (s *Server) actCreateMissing(ctx context.Context, _ Viewer, _ url.Values) (string, error) {
	return s.createMissing(ctx)
}

func (s *Server) actTrustBankMap(ctx context.Context, v Viewer, f url.Values) (string, error) {
	id, val := f.Get("trust_account_id"), strings.TrimSpace(f.Get("qbo_account"))
	if id == "" {
		return "", errStatus(http.StatusBadRequest, "trust_account_id names the trust account")
	}
	if val == "" {
		_, err := s.c.DB.ExecContext(ctx, `DELETE FROM trust_bank_map WHERE trust_account_id = $1`, id)
		return "Cleared.", err
	}
	_, err := s.c.DB.ExecContext(ctx, `INSERT INTO trust_bank_map (trust_account_id, qbo_account, set_by, set_at) VALUES ($1, $2, $3, $4)
ON CONFLICT (trust_account_id) DO UPDATE SET qbo_account = $2, set_by = $3, set_at = $4`, id, val, v.UserID, s.now().UnixMicro())
	if err != nil {
		return "", errStatus(http.StatusBadRequest, "that trust account is not in Togavsu")
	}
	return "Saved.", nil
}

func (s *Server) actClientSecret(ctx context.Context, _ Viewer, f url.Values) (string, error) {
	v := strings.TrimSpace(f.Get("client_secret"))
	if v == "" {
		return "", errStatus(http.StatusBadRequest, "enter the client secret")
	}
	return "Client secret saved.", s.core.putSecret(ctx, secretClient, v)
}

func (s *Server) actReset(ctx context.Context, _ Viewer, f url.Values) (string, error) {
	if f.Get("confirm") != "RESET" {
		return "", errStatus(http.StatusBadRequest, "type RESET to clear the sync tables")
	}
	return "Sync tables cleared; the sync log is kept.", s.store.reset(ctx)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// authorize verifies the viewer and the action's roles.
func (s *Server) authorize(r *http.Request, a action) (Viewer, error) {
	v, err := s.viewer(r)
	if err != nil {
		return Viewer{}, errStatus(http.StatusUnauthorized, "a context token is required")
	}
	if !v.Has(a.roles...) {
		return Viewer{}, errStatus(http.StatusForbidden, "only "+strings.Join(a.roles, " or ")+" can do this")
	}
	return v, nil
}

func (s *Server) perform(r *http.Request, a action, f url.Values) (string, error) {
	v, err := s.authorize(r, a)
	if err != nil {
		return "", err
	}
	s.work.Lock()
	defer s.work.Unlock()
	return a.run(s, r.Context(), v, f)
}

func (s *Server) formAction(a action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		msg, err := s.perform(r, a, r.PostForm)
		if err != nil {
			status, m := statusOf(err)
			http.Error(w, m, status)
			return
		}
		q := url.Values{"context": {contextToken(r)}, "done": {msg}}
		http.Redirect(w, r, a.tab+"?"+q.Encode(), http.StatusSeeOther)
	}
}

func (s *Server) apiAction(a action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f := url.Values{}
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/json" {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the body is a JSON object of strings"})
				return
			}
			for k, v := range body {
				f.Set(k, v)
			}
		} else if err := r.ParseForm(); err == nil {
			f = r.PostForm
		}
		msg, err := s.perform(r, a, f)
		if err != nil {
			status, m := statusOf(err)
			writeJSON(w, status, map[string]string{"error": m})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
	}
}

// openapi describes the agent endpoints.
func (s *Server) openapi(w http.ResponseWriter, _ *http.Request) {
	paths := map[string]any{}
	ctxParam := map[string]any{"name": "X-Togavsu-Context-Token", "in": "header", "required": true, "schema": map[string]string{"type": "string"}}
	for _, g := range []struct{ path, summary string }{
		{"/api/status", "Sync counts and connection state (admin, accountant)"},
		{"/api/books", "Exceptions and the books view; query status (synced, pending, exceptions, failed, drift, skipped) and month (YYYY-MM)"},
		{"/api/setup", "Connection, mapping checks, chart of accounts and the trust bank map"},
		{"/api/matter", "One matter's records with their QuickBooks state; query matter_id"},
	} {
		paths[g.path] = map[string]any{"get": map[string]any{"summary": g.summary, "parameters": []any{ctxParam},
			"responses": map[string]any{"200": map[string]string{"description": "OK"}, "404": map[string]string{"description": "Not for this viewer"}}}}
	}
	for _, a := range actions {
		props := map[string]any{}
		for _, p := range a.params {
			props[p] = map[string]string{"type": "string"}
		}
		schema := map[string]any{"type": "object", "properties": props}
		if len(a.params) > 0 {
			schema["required"] = a.params
		}
		paths["/api/"+a.path] = map[string]any{"post": map[string]any{
			"summary": a.summary + " (" + strings.Join(a.roles, ", ") + ")", "parameters": []any{ctxParam},
			"requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{
				"schema": schema}}},
			"responses": map[string]any{"200": map[string]string{"description": "Done"}, "403": map[string]string{"description": "Role not allowed"}},
		}}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"openapi": "3.0.3",
		"info":    map[string]string{"title": "QuickBooks Sync", "version": "1.0.0"},
		"servers": []map[string]string{{"url": "/m/" + ID}},
		"paths":   paths,
	})
}
