package quickbooks

import (
	"context"
	"database/sql"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
)

// Secret names in the core's sealed store. No module table, log line or response ever
// holds their values.
const (
	secretClient  = "qbo/client_secret"
	secretAccess  = "qbo/access_token"
	secretRefresh = "qbo/refresh_token"
)

// errPaused stops pushes while the connection needs an admin: items stay pending.
var errPaused = errors.New("quickbooks: the connection needs an admin to reconnect")

// tokenSource hands the adapter an access token, refreshing it with under five minutes
// left and storing every rotated refresh token.
type tokenSource struct {
	s  *Server
	st Settings
}

func (t *tokenSource) Token(ctx context.Context) (string, error) {
	s := t.s
	conn, err := s.store.connection(ctx)
	if err != nil {
		return "", err
	}
	if conn.State != "connected" {
		return "", errPaused
	}
	if conn.AccessExpiresAt-s.now().UnixMicro() < seconds(limits().RefreshMargin).Microseconds() {
		return t.Refresh(ctx)
	}
	s.tok.Lock()
	if s.tok.access != "" && s.tok.expires == conn.AccessExpiresAt {
		a := s.tok.access
		s.tok.Unlock()
		return a, nil
	}
	s.tok.Unlock()
	a, ok, err := s.core.getSecret(ctx, secretAccess)
	if err != nil {
		return "", err
	}
	if !ok {
		s.lost(ctx, "The QuickBooks sign-in is missing from the secret store. Reconnect QuickBooks.")
		return "", errPaused
	}
	s.cacheToken(a, conn.AccessExpiresAt)
	return a, nil
}

func (s *Server) cacheToken(a string, exp int64) {
	s.tok.Lock()
	s.tok.access, s.tok.expires = a, exp
	s.tok.Unlock()
}

func (t *tokenSource) Refresh(ctx context.Context) (string, error) {
	s := t.s
	ref, ok, err := s.core.getSecret(ctx, secretRefresh)
	if err != nil {
		return "", err
	}
	if !ok {
		s.lost(ctx, "The QuickBooks sign-in is missing from the secret store. Reconnect QuickBooks.")
		return "", errPaused
	}
	secret, _, err := s.core.getSecret(ctx, secretClient)
	if err != nil {
		return "", err
	}
	toks, err := s.c.QBO.Auth(t.st.Environment, t.st.ClientID, secret).Refresh(ctx, ref)
	if f := qbo.AsFault(err); f != nil {
		if f.Code == qbo.CodeInvalidGrant {
			s.lost(ctx, "QuickBooks refused the saved sign-in (invalid_grant). Reconnect QuickBooks.")
			return "", errPaused
		}
		return "", err
	}
	if err := s.storeTokens(ctx, toks); err != nil {
		return "", err
	}
	conn, err := s.store.connection(ctx)
	if err != nil {
		return "", err
	}
	conn.AccessExpiresAt, conn.RefreshExpiresAt = toks.AccessExpiresAt.UnixMicro(), toks.RefreshExpiresAt.UnixMicro()
	conn.LastRefreshAt = s.now().UnixMicro()
	if err := s.store.saveConnection(ctx, conn); err != nil {
		return "", err
	}
	s.cacheToken(toks.AccessToken, conn.AccessExpiresAt)
	return toks.AccessToken, nil
}

func (s *Server) storeTokens(ctx context.Context, t qbo.Tokens) error {
	if err := s.core.putSecret(ctx, secretAccess, t.AccessToken); err != nil {
		return err
	}
	return s.core.putSecret(ctx, secretRefresh, t.RefreshToken)
}

// lost marks the connection reconnect_needed and alerts the admins at once.
func (s *Server) lost(ctx context.Context, why string) {
	conn, err := s.store.connection(ctx)
	if err != nil || conn.State != "connected" {
		return
	}
	conn.State, conn.LastError = "reconnect_needed", why
	if err := s.store.saveConnection(ctx, conn); err != nil {
		s.log.Printf("connection: %v", err)
		return
	}
	s.cacheToken("", zero64)
	s.log.Printf("connection lost: reconnect needed")
	s.alertConnection(ctx, "QuickBooks needs to be reconnected", "connection")
}

// staticTokens serves a token pair fresh from the token endpoint.
type staticTokens struct{ access string }

func (t staticTokens) Token(context.Context) (string, error)   { return t.access, nil }
func (t staticTokens) Refresh(context.Context) (string, error) { return t.access, nil }

func redirectURI(r *http.Request) string {
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return "https://" + host + "/m/" + ID + "/oauth/callback"
}

// newNonce stores a one-use OAuth state bound to the admin who will press Connect.
func (s *Server) newNonce(ctx context.Context, userID, redirect string) (string, error) {
	n := randomToken()
	if _, err := s.c.DB.ExecContext(ctx, `DELETE FROM oauth_states WHERE expires_at < $1`, s.now().UnixMicro()); err != nil {
		return "", err
	}
	_, err := s.c.DB.ExecContext(ctx, `INSERT INTO oauth_states (state, user_id, redirect_uri, expires_at) VALUES ($1, $2, $3, $4)`,
		n, userID, redirect, s.now().Add(seconds(limits().StateSeconds)).UnixMicro())
	return n, err
}

func (s *Server) stateValid(ctx context.Context, n string) (string, bool) {
	var redirect string
	err := s.c.DB.QueryRowContext(ctx, `SELECT redirect_uri FROM oauth_states WHERE state = $1 AND used_at IS NULL AND expires_at > $2`,
		n, s.now().UnixMicro()).Scan(&redirect)
	return redirect, err == nil
}

var notice = template.Must(template.New("notice").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>QuickBooks</title></head><body>
<h1>{{.Title}}</h1><p>{{.Text}}</p></body></html>`))

func (s *Server) noticePage(w http.ResponseWriter, status int, title, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.WriteHeader(status)
	_ = notice.Execute(w, map[string]string{"Title": title, "Text": text})
}

// oauthStart runs in its own top-level window (Intuit will not load in a frame). The
// nonce, made for an admin when the setup tab rendered, is what authorizes it.
func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) {
	n := r.URL.Query().Get("nonce")
	redirect, ok := s.stateValid(r.Context(), n)
	if n == "" || !ok {
		s.noticePage(w, http.StatusForbidden, "Connect link expired", "This Connect link was used or has expired. Press Connect again on the QuickBooks setup tab.")
		return
	}
	st, err := s.settings(r.Context())
	if err != nil || st.ClientID == "" {
		s.noticePage(w, http.StatusConflict, "Not ready to connect", "Enter the Intuit client ID in the module settings first.")
		return
	}
	http.Redirect(w, r, s.c.QBO.Auth(st.Environment, st.ClientID, "").AuthCodeURL(n, redirect), http.StatusFound)
}

func (s *Server) settings(ctx context.Context) (Settings, error) {
	v, err := s.core.settings(ctx)
	if err != nil {
		return Settings{}, err
	}
	return parseSettings(v)
}

// oauthCallback checks the state, exchanges the code with the client secret from the
// secret store and stores the tokens there.
func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	var userID, redirect string
	err := s.c.DB.QueryRowContext(ctx, `UPDATE oauth_states SET used_at = $2 WHERE state = $1 AND used_at IS NULL AND expires_at > $2 RETURNING user_id, redirect_uri`,
		q.Get("state"), s.now().UnixMicro()).Scan(&userID, &redirect)
	if err != nil {
		s.noticePage(w, http.StatusForbidden, "Connect link expired", "This sign-in was used or has expired. Press Connect again on the QuickBooks setup tab.")
		return
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		s.noticePage(w, http.StatusBadRequest, "Not connected", "QuickBooks did not approve the connection.")
		return
	}
	st, err := s.settings(ctx)
	if err != nil {
		s.noticePage(w, http.StatusBadGateway, "Not connected", "The module can't read its settings right now.")
		return
	}
	secret, ok, err := s.core.getSecret(ctx, secretClient)
	if err != nil || !ok {
		s.noticePage(w, http.StatusConflict, "Not connected", "Enter the client secret on the setup tab first.")
		return
	}
	auth := s.c.QBO.Auth(st.Environment, st.ClientID, secret)
	toks, err := auth.Exchange(ctx, q.Get("code"), redirect)
	if err != nil {
		s.log.Printf("oauth: exchange refused")
		s.noticePage(w, http.StatusBadGateway, "Not connected", "QuickBooks refused the sign-in. Try Connect again.")
		return
	}
	if toks.RealmID == "" {
		toks.RealmID = q.Get("realmId")
	}
	s.work.Lock()
	defer s.work.Unlock()
	realms, err := s.store.syncRealms(ctx)
	if err != nil {
		s.noticePage(w, http.StatusInternalServerError, "Not connected", "The module's database is unavailable.")
		return
	}
	for _, rm := range realms {
		if rm != toks.RealmID {
			_ = auth.Revoke(ctx, toks.RefreshToken)
			s.noticePage(w, http.StatusConflict, "A different QuickBooks company",
				"The sync history belongs to another QuickBooks company. An admin must run Reset sync before connecting this one.")
			return
		}
	}
	if err := s.storeTokens(ctx, toks); err != nil {
		s.noticePage(w, http.StatusBadGateway, "Not connected", "The core's secret store refused the sign-in.")
		return
	}
	co, err := s.c.QBO.Open(qbo.Conn{Environment: st.Environment, RealmID: toks.RealmID}, staticTokens{toks.AccessToken}).Company(ctx)
	if err != nil {
		co = qbo.Company{RealmID: toks.RealmID}
	}
	conn, err := s.store.connection(ctx)
	if err != nil {
		s.noticePage(w, http.StatusInternalServerError, "Not connected", "The module's database is unavailable.")
		return
	}
	now := s.now()
	if conn.FirstConnectedOn == "" {
		conn.FirstConnectedOn = s.localDate(ctx, now)
	}
	conn.Environment, conn.RealmID, conn.CompanyName = st.Environment, toks.RealmID, co.Name
	conn.AccessExpiresAt, conn.RefreshExpiresAt = toks.AccessExpiresAt.UnixMicro(), toks.RefreshExpiresAt.UnixMicro()
	conn.State, conn.ConnectedBy, conn.ConnectedAt, conn.LastError = "connected", sql.NullString{String: userID, Valid: true}, now.UnixMicro(), ""
	if err := s.store.saveConnection(ctx, conn); err != nil {
		s.noticePage(w, http.StatusInternalServerError, "Not connected", "The module's database is unavailable.")
		return
	}
	s.cacheToken(toks.AccessToken, conn.AccessExpiresAt)
	s.log.Printf("connected to realm %s", toks.RealmID)
	s.noticePage(w, http.StatusOK, "Connected", "Connected to "+co.Name+". You can close this window.")
}

// localDate is today in the firm's time zone.
func (s *Server) localDate(ctx context.Context, at time.Time) string {
	p, err := s.core.profile(ctx)
	if err != nil {
		return at.UTC().Format(time.DateOnly)
	}
	return at.In(zone(p.TimeZone)).Format(time.DateOnly)
}

func zone(name string) *time.Location {
	if loc, err := time.LoadLocation(strings.TrimSpace(name)); err == nil && name != "" {
		return loc
	}
	return time.UTC
}

// disconnect revokes the refresh token at Intuit and deletes both token secrets; the
// sync rows stay.
func (s *Server) disconnect(ctx context.Context) error {
	st, err := s.settings(ctx)
	if err != nil {
		return err
	}
	if ref, ok, err := s.core.getSecret(ctx, secretRefresh); err == nil && ok {
		secret, _, _ := s.core.getSecret(ctx, secretClient)
		if err := s.c.QBO.Auth(st.Environment, st.ClientID, secret).Revoke(ctx, ref); err != nil {
			s.log.Printf("disconnect: revoke refused; tokens are deleted anyway")
		}
	}
	for _, n := range []string{secretAccess, secretRefresh} {
		if err := s.core.deleteSecret(ctx, n); err != nil {
			return err
		}
	}
	conn, err := s.store.connection(ctx)
	if err != nil {
		return err
	}
	conn.State, conn.AccessExpiresAt, conn.RefreshExpiresAt = "disconnected", zero64, zero64
	s.cacheToken("", zero64)
	return s.store.saveConnection(ctx, conn)
}
