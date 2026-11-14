// Command quickbooks runs the QuickBooks Sync module.
//
//	quickbooks serve -core http://core:8080 -secrets /run/secrets -db-host postgres:5432 -db-name togavsu
//	quickbooks package -key signing.key -out quickbooks.signed.json
//
// Secrets come from the Docker secrets the core hands the supervisor: togavsu_api_token,
// togavsu_webhook_secret, togavsu_db_role, togavsu_db_password and togavsu_db_schema.
// The Intuit client secret and the QuickBooks tokens are never here: the admin enters the
// client secret on the setup tab and the core's secret store keeps all three.
package main

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	_ "github.com/jackc/pgx/v5/stdlib"

	quickbooks "github.com/CharitonMedia/togavsu-quickbooks"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: quickbooks serve|package [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "package":
		pack(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: quickbooks serve|package [flags]")
		os.Exit(2)
	}
}

func secret(dir, name string) string {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		log.Fatalf("secret %s: %v", name, err)
	}
	return strings.TrimSpace(string(raw))
}

func optionalSecret(dir, name string) string {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "address to listen on")
	core := fs.String("core", "http://core:8080", "core base URL inside the stack")
	dir := fs.String("secrets", "/run/secrets", "directory of Docker secrets")
	host := fs.String("db-host", "postgres:5432", "Postgres host:port")
	name := fs.String("db-name", "togavsu", "database name")
	_ = fs.Parse(args)
	schema := optionalSecret(*dir, "togavsu_db_schema")
	if schema == "" {
		schema = "mod_" + quickbooks.ID
	}
	q := url.Values{"search_path": {schema}}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(secret(*dir, "togavsu_db_role"), secret(*dir, "togavsu_db_password")),
		Host: *host, Path: "/" + *name, RawQuery: q.Encode()}
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		log.Fatal(err)
	}
	srv := quickbooks.New(quickbooks.Config{Core: *core, Token: secret(*dir, "togavsu_api_token"),
		WebhookSecret: secret(*dir, "togavsu_webhook_secret"), DB: db, Log: os.Stderr})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.Run(ctx)
	hs := &http.Server{Addr: *listen, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		_ = hs.Shutdown(context.Background())
	}()
	log.Printf("quickbooks listening on %s", *listen)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// pack builds the signed package. Signing waits on a public signing package or CLI from
// the core; until then the stub packager reports that it is unavailable.
func pack(args []string) {
	fs := flag.NewFlagSet("package", flag.ExitOnError)
	keyFile := fs.String("key", "", "file holding a base64 ed25519 seed")
	out := fs.String("out", "quickbooks.signed.json", "signed package file to write")
	_ = fs.Parse(args)
	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		log.Fatal(err)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		log.Fatal("the key file must hold a base64 ed25519 seed")
	}
	signed, err := quickbooks.Build(quickbooks.StubPackager{}, ed25519.NewKeyFromSeed(seed))
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, signed, 0o644); err != nil {
		log.Fatal(err)
	}
}
