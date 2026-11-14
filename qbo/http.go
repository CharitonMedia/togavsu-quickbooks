package qbo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Endpoints are the Intuit addresses. They are adapter config, so tests point every one
// of them at an httptest server.
type Endpoints struct {
	Production string // API base for production companies
	Sandbox    string // API base for sandbox companies
	Authorize  string
	Token      string
	Revoke     string
}

// IntuitEndpoints are Intuit's published addresses.
func IntuitEndpoints() Endpoints {
	return Endpoints{
		Production: "https://quickbooks.api.intuit.com",
		Sandbox:    "https://sandbox-quickbooks.api.intuit.com",
		Authorize:  "https://appcenter.intuit.com/connect/oauth2",
		Token:      "https://oauth.platform.intuit.com/oauth2/v1/tokens/bearer",
		Revoke:     "https://developer.api.intuit.com/v2/oauth2/tokens/revoke",
	}
}

// Scope is the OAuth scope the module asks for.
const Scope = "com.intuit.quickbooks.accounting"

// HTTPProvider builds the Intuit adapter.
type HTTPProvider struct {
	Endpoints Endpoints
	HTTP      *http.Client
	// PerMinute and Concurrent hold each realm under Intuit's ceiling.
	PerMinute  int
	Concurrent int
	Now        func() time.Time

	mu       sync.Mutex
	limiters map[string]*Limiter
}

func (p *HTTPProvider) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return http.DefaultClient
}

// BaseURL is the API base for environment.
func (p *HTTPProvider) BaseURL(environment string) string {
	if environment == "production" {
		return p.Endpoints.Production
	}
	return p.Endpoints.Sandbox
}

// Auth returns the OAuth client for the firm's Intuit app.
func (p *HTTPProvider) Auth(environment, clientID, clientSecret string) Auth {
	return &httpAuth{ep: p.Endpoints, hc: p.client(), id: clientID, secret: clientSecret, now: p.now}
}

func (p *HTTPProvider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Open binds the adapter to one company.
func (p *HTTPProvider) Open(conn Conn, ts TokenSource) Books {
	p.mu.Lock()
	if p.limiters == nil {
		p.limiters = map[string]*Limiter{}
	}
	l, ok := p.limiters[conn.RealmID]
	if !ok {
		l = NewLimiter(p.PerMinute, p.Concurrent, p.now)
		p.limiters[conn.RealmID] = l
	}
	p.mu.Unlock()
	return &httpBooks{base: strings.TrimRight(p.BaseURL(conn.Environment), "/"), realm: conn.RealmID, ts: ts, hc: p.client(), lim: l}
}

type httpAuth struct {
	ep         Endpoints
	hc         *http.Client
	id, secret string
	now        func() time.Time
}

func (a *httpAuth) AuthCodeURL(state, redirectURI string) string {
	q := url.Values{"client_id": {a.id}, "response_type": {"code"}, "scope": {Scope}, "redirect_uri": {redirectURI}, "state": {state}}
	return a.ep.Authorize + "?" + q.Encode()
}

type tokenReply struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshIn    int64  `json:"x_refresh_token_expires_in"`
	Error        string `json:"error"`
}

func (a *httpAuth) tokenCall(ctx context.Context, form url.Values) (Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.ep.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.SetBasicAuth(a.id, a.secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return Tokens{}, &Fault{Code: CodeNetwork, Message: "token endpoint unreachable", Retryable: true}
	}
	defer resp.Body.Close()
	var tr tokenReply
	_ = json.NewDecoder(resp.Body).Decode(&tr)
	if resp.StatusCode != http.StatusOK {
		code := tr.Error
		if code == "" {
			code = strconv.Itoa(resp.StatusCode)
		}
		return Tokens{}, &Fault{HTTPStatus: resp.StatusCode, Code: code, Message: "token request refused", Retryable: RetryableStatus(resp.StatusCode)}
	}
	now := a.now()
	return Tokens{AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken,
		AccessExpiresAt:  now.Add(time.Duration(tr.ExpiresIn) * second),
		RefreshExpiresAt: now.Add(time.Duration(tr.RefreshIn) * second)}, nil
}

func (a *httpAuth) Exchange(ctx context.Context, code, redirectURI string) (Tokens, error) {
	return a.tokenCall(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}})
}

func (a *httpAuth) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	return a.tokenCall(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
}

func (a *httpAuth) Revoke(ctx context.Context, token string) error {
	raw, _ := json.Marshal(map[string]string{"token": token})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.ep.Revoke, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.SetBasicAuth(a.id, a.secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return &Fault{Code: CodeNetwork, Message: "revoke endpoint unreachable", Retryable: true}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return &Fault{HTTPStatus: resp.StatusCode, Code: strconv.Itoa(resp.StatusCode), Message: "revoke refused"}
	}
	return nil
}

type httpBooks struct {
	base, realm string
	ts          TokenSource
	hc          *http.Client
	lim         *Limiter
}

type faultBody struct {
	Fault struct {
		Error []struct {
			Message string `json:"Message"`
			Detail  string `json:"Detail"`
			Code    string `json:"code"`
		} `json:"Error"`
		Type string `json:"type"`
	} `json:"Fault"`
}

// call sends one request. A 401 makes it refresh the token once and repeat.
func (b *httpBooks) call(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return err
		}
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("minorversion", MinorVersion)
	u := b.base + "/v3/company/" + url.PathEscape(b.realm) + path + "?" + q.Encode()
	tok, err := b.ts.Token(ctx)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		status, resp, err := b.send(ctx, method, u, tok, raw)
		if err != nil {
			return &Fault{Code: CodeNetwork, Message: "QuickBooks did not answer", Retryable: true}
		}
		if status == http.StatusUnauthorized && attempt == zero {
			if tok, err = b.ts.Refresh(ctx); err != nil {
				return err
			}
			continue
		}
		if status >= http.StatusOK && status < http.StatusMultipleChoices {
			if out == nil {
				return nil
			}
			return json.Unmarshal(resp, out)
		}
		return parseFault(status, resp)
	}
}

func (b *httpBooks) send(ctx context.Context, method, u, tok string, raw []byte) (int, []byte, error) {
	release, err := b.lim.Acquire(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer release()
	var rd io.Reader
	if raw != nil {
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

func parseFault(status int, body []byte) error {
	var fb faultBody
	_ = json.Unmarshal(body, &fb)
	f := &Fault{HTTPStatus: status, Code: strconv.Itoa(status), Message: http.StatusText(status), Retryable: RetryableStatus(status)}
	if len(fb.Fault.Error) > 0 {
		e := fb.Fault.Error[0]
		if e.Code != "" {
			f.Code = e.Code
		}
		f.Message = strings.TrimSpace(e.Message + ": " + e.Detail)
	}
	return f
}

func entityPath(k Kind) string { return "/" + strings.ToLower(string(k)) }

// unwrap reads {"Invoice": {...}} into e.
func unwrap(k Kind, raw map[string]json.RawMessage, e any) error {
	inner, ok := raw[string(k)]
	if !ok {
		return &Fault{Code: "decode", Message: "QuickBooks answer has no " + string(k)}
	}
	return json.Unmarshal(inner, e)
}

func (b *httpBooks) write(ctx context.Context, k Kind, q url.Values, body any, requestID string) (Ref, error) {
	if q == nil {
		q = url.Values{}
	}
	q.Set("requestid", requestID)
	var raw map[string]json.RawMessage
	if err := b.call(ctx, http.MethodPost, entityPath(k), q, body, &raw); err != nil {
		return Ref{}, err
	}
	var h Head
	if err := unwrap(k, raw, &h); err != nil {
		return Ref{}, err
	}
	return h.GetRef(), nil
}

func (b *httpBooks) Create(ctx context.Context, e Entity, requestID string) (Ref, error) {
	return b.write(ctx, e.Kind(), nil, e, requestID)
}

func (b *httpBooks) Update(ctx context.Context, e Entity, requestID string) (Ref, error) {
	return b.write(ctx, e.Kind(), nil, e, requestID)
}

func (b *httpBooks) Void(ctx context.Context, k Kind, ref Ref, requestID string) (Ref, error) {
	body := map[string]any{"Id": ref.ID, "SyncToken": ref.SyncToken}
	q := url.Values{"operation": {"void"}}
	if k == KindPayment {
		q = url.Values{"operation": {"update"}, "include": {"void"}}
		body["sparse"] = true
	}
	return b.write(ctx, k, q, body, requestID)
}

func (b *httpBooks) Delete(ctx context.Context, k Kind, ref Ref, requestID string) error {
	q := url.Values{"operation": {"delete"}, "requestid": {requestID}}
	return b.call(ctx, http.MethodPost, entityPath(k), q, map[string]any{"Id": ref.ID, "SyncToken": ref.SyncToken}, nil)
}

func (b *httpBooks) Read(ctx context.Context, k Kind, id string) (Entity, Ref, error) {
	var raw map[string]json.RawMessage
	if err := b.call(ctx, http.MethodGet, entityPath(k)+"/"+url.PathEscape(id), nil, nil, &raw); err != nil {
		return nil, Ref{}, err
	}
	e, err := New(k)
	if err != nil {
		return nil, Ref{}, err
	}
	if err := unwrap(k, raw, e); err != nil {
		return nil, Ref{}, err
	}
	return e, e.GetRef(), nil
}

func (b *httpBooks) Company(ctx context.Context) (Company, error) {
	var raw map[string]json.RawMessage
	if err := b.call(ctx, http.MethodGet, "/companyinfo/"+url.PathEscape(b.realm), nil, nil, &raw); err != nil {
		return Company{}, err
	}
	var ci struct {
		CompanyName string `json:"CompanyName"`
	}
	if err := unwrap("CompanyInfo", raw, &ci); err != nil {
		return Company{}, err
	}
	return Company{RealmID: b.realm, Name: ci.CompanyName}, nil
}

// query runs a QBO SQL query, page by page, appending each page's entity list to out.
func query[T any](ctx context.Context, b *httpBooks, entity, where string) ([]T, error) {
	var out []T
	for start := firstPosition; ; start += pageSize {
		q := fmt.Sprintf("select * from %s%s startposition %d maxresults %d", entity, where, start, pageSize)
		var raw struct {
			QueryResponse map[string]json.RawMessage `json:"QueryResponse"`
		}
		if err := b.call(ctx, http.MethodGet, "/query", url.Values{"query": {q}}, nil, &raw); err != nil {
			return nil, err
		}
		var page []T
		if list, ok := raw.QueryResponse[entity]; ok {
			if err := json.Unmarshal(list, &page); err != nil {
				return nil, err
			}
		}
		out = append(out, page...)
		if len(page) < pageSize {
			return out, nil
		}
	}
}

func (b *httpBooks) Accounts(ctx context.Context) ([]Account, error) {
	return query[Account](ctx, b, "Account", "")
}

func (b *httpBooks) Items(ctx context.Context) ([]Item, error) {
	return query[Item](ctx, b, "Item", "")
}

func (b *httpBooks) PaymentMethods(ctx context.Context) ([]PaymentMethod, error) {
	return query[PaymentMethod](ctx, b, "PaymentMethod", "")
}

func (b *httpBooks) CustomerByName(ctx context.Context, displayName string) (*Customer, error) {
	where := " where DisplayName = '" + strings.ReplaceAll(displayName, "'", `\'`) + "'"
	list, err := query[Customer](ctx, b, "Customer", where)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

func (b *httpBooks) Changes(ctx context.Context, kinds []Kind, since time.Time) ([]Change, error) {
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}
	q := url.Values{"entities": {strings.Join(names, ",")}, "changedSince": {since.UTC().Format(time.RFC3339)}}
	var raw struct {
		CDCResponse []struct {
			QueryResponse []map[string]json.RawMessage `json:"QueryResponse"`
		} `json:"CDCResponse"`
	}
	if err := b.call(ctx, http.MethodGet, "/cdc", q, nil, &raw); err != nil {
		return nil, err
	}
	var out []Change
	for _, r := range raw.CDCResponse {
		for _, qr := range r.QueryResponse {
			for _, k := range kinds {
				list, ok := qr[string(k)]
				if !ok {
					continue
				}
				var rows []struct {
					ID        string `json:"Id"`
					SyncToken string `json:"SyncToken"`
					Status    string `json:"status"`
					MetaData  struct {
						LastUpdatedTime time.Time `json:"LastUpdatedTime"`
					} `json:"MetaData"`
				}
				if err := json.Unmarshal(list, &rows); err != nil {
					return nil, err
				}
				for _, row := range rows {
					out = append(out, Change{Kind: k, ID: row.ID, SyncToken: row.SyncToken, Deleted: row.Status == "Deleted", UpdatedAt: row.MetaData.LastUpdatedTime})
				}
			}
		}
	}
	return out, nil
}

func (b *httpBooks) CreateAccount(ctx context.Context, a Account, requestID string) (Ref, error) {
	return b.write(ctx, "Account", nil, a, requestID)
}

func (b *httpBooks) CreateItem(ctx context.Context, i Item, requestID string) (Ref, error) {
	return b.write(ctx, "Item", nil, i, requestID)
}

// Limiter holds one realm to a number of requests per rolling minute and a number at
// once. It waits rather than fails; it sets no deadline of its own.
type Limiter struct {
	mu        sync.Mutex
	perMinute int
	sem       chan struct{}
	stamps    []time.Time
	now       func() time.Time
	// Waits counts the times a request waited for the per-minute ceiling (tests read it).
	Waits int
	sleep func(context.Context, time.Duration) error
}

// NewLimiter returns a limiter; now may be nil for the wall clock.
func NewLimiter(perMinute, concurrent int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{perMinute: perMinute, sem: make(chan struct{}, concurrent), now: now, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SetSleep replaces how the limiter waits (tests advance a fake clock instead).
func (l *Limiter) SetSleep(f func(context.Context, time.Duration) error) { l.sleep = f }

// InFlight is how many requests hold a slot now.
func (l *Limiter) InFlight() int { return len(l.sem) }

// Acquire takes a slot and returns its release.
func (l *Limiter) Acquire(ctx context.Context) (func(), error) {
	for {
		l.mu.Lock()
		now := l.now()
		cut := now.Add(-rateWindow)
		keep := l.stamps[:0]
		for _, s := range l.stamps {
			if s.After(cut) {
				keep = append(keep, s)
			}
		}
		l.stamps = keep
		if len(l.stamps) < l.perMinute {
			l.stamps = append(l.stamps, now)
			l.mu.Unlock()
			break
		}
		wait := l.stamps[0].Add(rateWindow).Sub(now)
		l.Waits++
		l.mu.Unlock()
		if err := l.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return func() { <-l.sem }, nil
}
