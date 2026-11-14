package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"
)

// TestPackWritesSignedPackage runs quickbooks package with a throwaway seed and checks
// that it writes a signed package the core verifies and installs.
func TestPackWritesSignedPackage(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key, out := filepath.Join(dir, "signing.key"), filepath.Join(dir, "quickbooks.signed.json")
	if err := os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(priv.Seed())), 0o600); err != nil {
		t.Fatal(err)
	}
	pack([]string{"-key", key, "-out", out})
	signed, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := modulekit.VerifyFile(signed, []modulekit.TrustedKey{modulekit.Trust(priv.Public().(ed25519.PublicKey))})
	if err != nil {
		t.Fatalf("verify: %v (%s)", err, modulekit.Code(err))
	}
	if _, _, err := modulekit.CheckPackage(v.Package); err != nil {
		t.Fatalf("install check: %v (%s)", err, modulekit.Code(err))
	}
}
