package qbo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Synthetic QBO JSON only; every base URL points at an httptest server.

type fakeTokens struct {
	mu        sync.Mutex
	tok       string
	refreshes int
}

func (f *fakeTokens) Token(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tok, nil
}

func (f *fakeTokens) Refresh(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshes++
	f.tok = "access-after-refresh"
	return f.tok, nil
}

func provider(srv *httptest.Server) *HTTPProvider {
	return &HTTPProvider{Endpoints: Endpoints{Production: srv.URL + "/prod", Sandbox: srv.URL + "/sandbox", Authorize: srv.URL + "/authorize",
		Token: srv.URL + "/token", Revoke: srv.URL + "/revoke"}, HTTP: srv.Client(), PerMinute: 400, Concurrent: 4}
}

func TestWriteSendsRequestIDAndMinorVersion(t *testing.T) {
	var got *http.Request
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"Invoice":{"Id":"130","SyncToken":"0"}}`)
	}))
	defer srv.Close()
	b := provider(srv).Open(Conn{Environment: "sandbox", RealmID: "4620816365"}, &fakeTokens{tok: "access-1"})
	ref, err := b.Create(context.Background(), &Invoice{CustomerRef: RefVal{Value: "58"}, Line: []Line{{Amount: 12345}}, EmailStatus: "NotSet"}, "req-1")
	if err != nil || ref.ID != "130" || ref.SyncToken != "0" {
		t.Fatalf("%v %+v", err, ref)
	}
	if got.URL.Path != "/sandbox/v3/company/4620816365/invoice" || got.URL.Query().Get("minorversion") != "75" || got.URL.Query().Get("requestid") != "req-1" {
		t.Fatalf("url %s", got.URL)
	}
	if got.Header.Get("Authorization") != "Bearer access-1" {
		t.Fatal("bearer")
	}
	lines := body["Line"].([]any)
	if lines[0].(map[string]any)["Amount"].(float64) != 123.45 || body["EmailStatus"] != "NotSet" {
		t.Fatalf("body %v", body)
	}
}

func TestUnauthorizedRefreshesOnce(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer access-after-refresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"CompanyInfo":{"CompanyName":"Sample Law Co"}}`)
	}))
	defer srv.Close()
	ts := &fakeTokens{tok: "stale"}
	co, err := provider(srv).Open(Conn{Environment: "production", RealmID: "1"}, ts).Company(context.Background())
	if err != nil || co.Name != "Sample Law Co" || ts.refreshes != 1 || calls != 2 {
		t.Fatalf("%v %+v refreshes=%d calls=%d", err, co, ts.refreshes, calls)
	}
}

func TestFaults(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		code      string
		retryable bool
	}{
		{400, `{"Fault":{"Error":[{"Message":"Duplicate Name Exists Error","Detail":"The name supplied already exists.","code":"6240"}],"type":"ValidationFault"}}`, "6240", false},
		{400, `{"Fault":{"Error":[{"Message":"Stale Object Error","Detail":"stale","code":"5010"}],"type":"ValidationFault"}}`, "5010", false},
		{429, ``, "429", true},
		{503, `<html>down</html>`, "503", true},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		}))
		_, err := provider(srv).Open(Conn{RealmID: "1"}, &fakeTokens{tok: "a"}).Create(context.Background(), &Customer{DisplayName: "Jane Sample"}, "r")
		srv.Close()
		f := AsFault(err)
		if f == nil || f.Code != c.code || f.Retryable != c.retryable || f.HTTPStatus != c.status {
			t.Fatalf("%d: %+v", c.status, f)
		}
	}
}

func TestTokenExchangeAndInvalidGrant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		_ = r.ParseForm()
		if !ok || id != "client-1" || secret == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Form.Get("grant_type") == "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"a1","refresh_token":"r1","expires_in":3600,"x_refresh_token_expires_in":8726400}`)
	}))
	defer srv.Close()
	p := provider(srv)
	now := time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return now }
	a := p.Auth("sandbox", "client-1", "s-"+strings.Repeat("x", 4))
	tk, err := a.Exchange(context.Background(), "code", "https://firm.example.test/m/quickbooks/oauth/callback")
	if err != nil || tk.AccessToken != "a1" || !tk.AccessExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("%v %+v", err, tk)
	}
	if _, err := a.Refresh(context.Background(), "r1"); AsFault(err).Code != CodeInvalidGrant {
		t.Fatalf("refresh %v", err)
	}
	if u := a.AuthCodeURL("st", "https://x/cb"); !strings.HasPrefix(u, srv.URL+"/authorize?") || !strings.Contains(u, "scope=com.intuit.quickbooks.accounting") {
		t.Fatal(u)
	}
}

func TestLimiter(t *testing.T) {
	now := time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	l := NewLimiter(3, 2, clock)
	l.SetSleep(func(_ context.Context, d time.Duration) error { mu.Lock(); now = now.Add(d); mu.Unlock(); return nil })
	for i := 0; i < 3; i++ {
		rel, err := l.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		rel()
	}
	if l.Waits != 0 {
		t.Fatal("waited early")
	}
	rel, _ := l.Acquire(context.Background())
	rel()
	if l.Waits != 1 || !clock().Equal(time.Date(2026, 3, 2, 15, 1, 0, 0, time.UTC)) {
		t.Fatalf("waits %d clock %v", l.Waits, clock())
	}
	a, _ := l.Acquire(context.Background())
	b, _ := l.Acquire(context.Background())
	if l.InFlight() != 2 {
		t.Fatal("in flight")
	}
	a()
	b()
}

func TestMoney(t *testing.T) {
	for in, want := range map[string]Money{"123.45": 12345, "-0.05": -5, "10": 1000, "0.1": 10} {
		got, err := ParseMoney(in)
		if err != nil || got != want {
			t.Fatalf("%s: %v %v", in, got, err)
		}
	}
	if _, err := ParseMoney("1.005"); err == nil {
		t.Fatal("three decimals accepted")
	}
	if Money(-1234).String() != "-12.34" {
		t.Fatal(Money(-1234).String())
	}
}
