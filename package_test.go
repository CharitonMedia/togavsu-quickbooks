package quickbooks_test

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	quickbooks "github.com/CharitonMedia/togavsu-quickbooks"
	"github.com/CharitonMedia/togavsu-quickbooks/internal/pendingcore"
	"github.com/CharitonMedia/togavsu/pkg/modulekit"
)

// TestPendingFieldsStillRefused asserts the pinned core's install check refuses the full
// package for each field pendingcore lists and for nothing else. It fails once the core
// accepts a field: drop its entry then, and bring back the togavsu-module check step in CI
// when the list's checked fields are gone.
func TestPendingFieldsStillRefused(t *testing.T) {
	full, err := quickbooks.Files()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := modulekit.ReadDir("package")
	if err != nil {
		t.Fatal(err)
	}
	if len(dir) != len(full) {
		t.Fatalf("package dir has %d files, the embedded package %d", len(dir), len(full))
	}
	for name, b := range full {
		if !bytes.Equal(dir[name], b) {
			t.Fatalf("package/%s differs from the embedded copy", name)
		}
	}
	checked, scopes := pendingcore.Checked()
	for _, f := range pendingcore.Fields {
		if !f.Present(full) {
			t.Fatalf("%s (%s) is listed as pending but the package no longer has it", f.Name, f.Item)
		}
	}
	for i, f := range checked {
		var others []pendingcore.Field
		others = append(others, checked[:i]...)
		others = append(others, checked[i+1:]...)
		_, _, err := modulekit.Check(pendingcore.Strip(full, others...))
		if err == nil {
			t.Fatalf("the core now accepts %s: drop its %s entry from internal/pendingcore", f.Name, f.Item)
		}
		if !f.Refusal.MatchString(err.Error()) {
			t.Fatalf("with only %s (%s) left, the check refused something else: %v", f.Name, f.Item, err)
		}
	}
	if _, _, err := modulekit.Check(pendingcore.Strip(full, checked...)); err != nil {
		t.Fatalf("the package minus the pending fields is refused: %v (%s)", err, modulekit.Code(err))
	}
	for _, f := range scopes {
		t.Logf("%s waits for %s; the install check passes it and approval refuses it until then", f.Name, f.Item)
	}
}

// TestPackageSignsAndVerifies archives, signs with a throwaway key and verifies the package
// minus the pending fields the way the core does at install. Once the list is empty it
// covers the shipped package itself.
func TestPackageSignsAndVerifies(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	full, err := quickbooks.Files()
	if err != nil {
		t.Fatal(err)
	}
	checked, _ := pendingcore.Checked()
	files := pendingcore.Strip(full, checked...)
	tar, err := modulekit.Archive(files)
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := modulekit.CheckPackage(tar)
	if err != nil {
		t.Fatalf("check package: %v (%s)", err, modulekit.Code(err))
	}
	if pkg.Manifest.ID != quickbooks.ID {
		t.Fatalf("manifest id %q", pkg.Manifest.ID)
	}
	signed, err := quickbooks.SignFile(priv, files)
	if err != nil {
		t.Fatal(err)
	}
	v, ms, err := modulekit.VerifyFile(signed, []modulekit.TrustedKey{modulekit.Trust(pub)})
	if err != nil {
		t.Fatalf("verify: %v (%s)", err, modulekit.Code(err))
	}
	if v.ModuleID != quickbooks.ID || len(ms) != 3 {
		t.Fatalf("verified %s with %d migrations", v.ModuleID, len(ms))
	}
	other, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := modulekit.VerifyFile(signed, []modulekit.TrustedKey{modulekit.Trust(other)}); err == nil {
		t.Fatal("a package signed by an untrusted key verified")
	}
	if len(checked) == 0 {
		// The shipped package itself, as quickbooks package builds it.
		if signed, err = quickbooks.Build(priv); err != nil {
			t.Fatal(err)
		}
		if _, _, err := modulekit.VerifyFile(signed, []modulekit.TrustedKey{modulekit.Trust(pub)}); err != nil {
			t.Fatalf("verify the shipped package: %v", err)
		}
	}
}
