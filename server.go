package quickbooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/CharitonMedia/togavsu-quickbooks/internal/wire"
	"github.com/CharitonMedia/togavsu-quickbooks/qbo"
)

// Config is what the container gets: the core's address, its secrets, its own database
// login and the QuickBooks provider (the Intuit adapter, or the fake in tests).
type Config struct {
	Core          string
	Token         string
	WebhookSecret string
	DB            *sql.DB
	HTTP          *http.Client
	QBO           qbo.Provider
	Now           func() time.Time
	// Log receives operational lines. It never gets a token or the client secret.
	Log io.Writer
}

// Server is the module.
type Server struct {
	c     Config
	core  *core
	store *store
	log   *log.Logger
	// work serializes the worker, the reconcile and the actions that change rows.
	work sync.Mutex
	tok  struct {
		sync.Mutex
		access  string
		expires int64
	}
}

// New returns the module server.
func New(c Config) *Server {
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = io.Discard
	}
	if c.QBO == nil {
		c.QBO = &qbo.HTTPProvider{Endpoints: qbo.IntuitEndpoints(), HTTP: c.HTTP, PerMinute: limits().PerMinute,
			Concurrent: limits().Concurrent, Now: c.Now}
	}
	return &Server{c: c, core: &core{base: c.Core, token: c.Token, hc: c.HTTP, pageLimit: limits().CorePageLimit},
		store: &store{db: c.DB}, log: log.New(c.Log, "quickbooks ", 0)}
}

func (s *Server) now() time.Time { return s.c.Now().UTC() }

// Handler serves the screens, the agent API, OAuth and the event webhook.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /widget", s.page(s.widgetData, "widget"))
	m.HandleFunc("GET /books", s.page(s.booksData, "books"))
	m.HandleFunc("GET /setup", s.page(s.setupData, "setup"))
	m.HandleFunc("GET /matter", s.page(s.matterData, "matter"))
	m.HandleFunc("GET /api/status", s.api(s.widgetData))
	m.HandleFunc("GET /api/books", s.api(s.booksData))
	m.HandleFunc("GET /api/setup", s.api(s.setupData))
	m.HandleFunc("GET /api/matter", s.api(s.matterData))
	for _, a := range actions {
		m.HandleFunc("POST /"+a.path, s.formAction(a))
		m.HandleFunc("POST /api/"+a.path, s.apiAction(a))
	}
	m.HandleFunc("GET /api/openapi.json", s.openapi)
	m.HandleFunc("GET /oauth/start", s.oauthStart)
	m.HandleFunc("GET /oauth/callback", s.oauthCallback)
	m.HandleFunc("POST /events", s.events)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	return m
}

// Run drives the worker every tick until ctx ends.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(seconds(limits().TickSeconds))
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil {
			s.log.Printf("worker: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// contextToken finds the frame's or agent's context token.
func contextToken(r *http.Request) string {
	if t := r.Header.Get(wire.ContextHeader); t != "" {
		return t
	}
	if t := r.URL.Query().Get("context"); t != "" {
		return t
	}
	if r.Method == http.MethodPost {
		return r.FormValue("context")
	}
	return ""
}

func (s *Server) viewer(r *http.Request) (Viewer, error) {
	tok := contextToken(r)
	if tok == "" {
		return Viewer{}, fmt.Errorf("no context token")
	}
	return s.core.verify(r.Context(), tok)
}

// httpError is an error with the status the module answers.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func errStatus(status int, msg string) error { return &httpError{status: status, msg: msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func statusOf(err error) (int, string) {
	if he, ok := err.(*httpError); ok {
		return he.status, he.msg
	}
	if isNotFound(err) {
		return http.StatusNotFound, "not found"
	}
	return http.StatusBadGateway, "QuickBooks Sync can't reach the core right now"
}

// events receives the core's signed webhooks: verified, recorded once, and turned into a
// pending sync row.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, limits().EventBodyBytes))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := wire.VerifyWebhook(s.c.WebhookSecret, r.Header.Get(wire.SignatureHeader), body, s.now(), seconds(limits().SkewSeconds)); err != nil {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var ev wire.EventPayload
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
	if err := s.receive(r.Context(), ev); err != nil {
		s.log.Printf("event %s: %v", ev.Action, err)
		http.Error(w, "not stored", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) receive(ctx context.Context, ev wire.EventPayload) error {
	now := s.now().UnixMicro()
	var customers []*Row
	if ev.Action == "contact.updated" {
		var err error
		if customers, err = s.store.customerRows(ctx, ev.RecordID); err != nil {
			return err
		}
	}
	// The event is recorded and its rows queued together, so an event whose queueing
	// failed is taken again when the core redelivers it.
	tx, err := s.c.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO received_events (event_id, type, record_id, received_at) VALUES ($1, $2, $3, $4) ON CONFLICT (event_id) DO NOTHING`,
		ev.ID, ev.Action, ev.RecordID, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // delivered before
	}
	prefix, _, _ := strings.Cut(ev.Action, ".")
	switch {
	case prefix == "invoice":
		err = queueOn(ctx, tx, tInvoice, ev.RecordID, now)
	case prefix == "payment":
		err = queueOn(ctx, tx, tPayment, ev.RecordID, now)
	case prefix == "expense":
		err = queueOn(ctx, tx, tExpense, ev.RecordID, now)
	case ev.Action == "trust.entry_posted":
		err = queueOn(ctx, tx, tTrust, ev.RecordID, now)
	case ev.Action == "contact.updated":
		for _, r := range customers {
			if err = queueOn(ctx, tx, tCustomer, r.Key, now); err != nil {
				break
			}
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
