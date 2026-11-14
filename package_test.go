package quickbooks_test

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"testing"

	quickbooks "github.com/CharitonMedia/togavsu-quickbooks"
	"github.com/CharitonMedia/togavsu/pkg/modulekit"
)

// TestPackageDirIsEmbedded checks that the embedded package is the package directory on
// disk, which togavsu-module reads.
func TestPackageDirIsEmbedded(t *testing.T) {
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
}

// TestPackageSignsAndVerifies builds the package the way quickbooks package does, signs
// it with a key made at test time and checks the signed file the way the core does at
// install. Build must succeed with a valid key whatever the pinned core accepts: it fails
// when the core refuses a part pending.yaml doesn't list. The test also fails when a
// pending entry holds nothing back (the core now accepts it: drop the entry), and when a
// held part is refused for anything but a field, scope or core table the core doesn't
// know yet.
func TestPackageSignsAndVerifies(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signed, held, err := quickbooks.Build(priv)
	if err != nil {
		var refs modulekit.Refusals
		if errors.As(err, &refs) {
			for _, r := range refs {
				t.Errorf("refused and not in pending.yaml: %s %s%s: %s: %s", r.File, r.Path, statement(r), r.Code, r.Message)
			}
		}
		t.Fatalf("build with a valid key: %v (%s)", err, modulekit.Code(err))
	}
	entries, err := quickbooks.Pending()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range modulekit.Unused(entries, held) {
		t.Errorf("pending %s (%s) holds nothing back: the pinned core accepts it or the package no longer has it; drop its entry", e.Part(), e.Core)
	}
	for _, h := range held {
		t.Log(h.String())
		for _, r := range h.Refusals {
			switch r.Code {
			case modulekit.RefuseUnknownField, modulekit.RefuseUnknownScope, modulekit.RefuseCoreReference:
			default:
				t.Errorf("%s is held back for %s, which no core item fixes: %s", h.Part, r.Code, r.Message)
			}
		}
	}

	v, ms, err := modulekit.VerifyFile(signed, []modulekit.TrustedKey{modulekit.Trust(pub)})
	if err != nil {
		t.Fatalf("verify: %v (%s)", err, modulekit.Code(err))
	}
	if v.ModuleID != quickbooks.ID || len(ms) != 3 {
		t.Fatalf("verified %s with %d migrations", v.ModuleID, len(ms))
	}
	pkg, _, err := modulekit.CheckPackage(v.Package)
	if err != nil {
		t.Fatalf("the signed package fails the install check: %v (%s)", err, modulekit.Code(err))
	}
	if refs := modulekit.CheckAll(pkg.Files); len(refs) != 0 {
		t.Fatalf("the signed package's files are refused: %v", refs)
	}
	if _, ok := pkg.Files[modulekit.PendingFile]; ok {
		t.Fatal("the signed package ships pending.yaml")
	}

	other, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := modulekit.VerifyFile(signed, []modulekit.TrustedKey{modulekit.Trust(other)}); err == nil {
		t.Fatal("a package signed by an untrusted key verified")
	}
	tar, err := quickbooks.Archive()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tar, v.Package) {
		t.Fatal("Archive is not the package Build signed")
	}
}

func statement(r modulekit.Refusal) string {
	if r.Statement == 0 {
		return ""
	}
	return fmt.Sprintf(" statement %d", r.Statement)
}
