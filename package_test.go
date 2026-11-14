package quickbooks_test

import (
	"crypto/ed25519"
	"regexp"
	"sort"
	"strings"
	"testing"

	quickbooks "github.com/CharitonMedia/togavsu-quickbooks"
	"github.com/CharitonMedia/togavsu/pkg/modulekit"
)

// pendingCore maps the manifest and migration features the pinned core's install check
// refuses to the CORE item that adds them, with the edit that removes the feature.
var pendingCore = []struct {
	refusal *regexp.Regexp
	item    string
	file    string
	strip   *regexp.Regexp
}{
	{regexp.MustCompile(`unknown field "notifications"`), "CORE-03", "togavsu-module.yaml", regexp.MustCompile(`(?m)^notifications:\n(?:  .*\n)+`)},
	{regexp.MustCompile(`unknown field "(section|icon|order|subgroup|badge)"`), "CORE-08", "togavsu-module.yaml", regexp.MustCompile(`(?m)^    (section|icon|order|subgroup|badge): .*\n`)},
	{regexp.MustCompile(`core\.trust_accounts is not a core record`), "CORE-22", "migrations/0001_connection.sql", regexp.MustCompile(` REFERENCES core\.trust_accounts \(id\) ON DELETE RESTRICT`)},
}

// checked runs the install check on files. Each refusal a pending CORE item explains is
// stripped and recorded, so the rest of the package is still checked and signed; any other
// refusal fails the test.
func checked(t *testing.T, files map[string][]byte) (map[string][]byte, []string) {
	t.Helper()
	files = clone(files)
	var items []string
	for {
		_, _, err := modulekit.Check(files)
		if err == nil {
			sort.Strings(items)
			return files, items
		}
		matched := false
		for _, p := range pendingCore {
			if p.refusal.MatchString(err.Error()) && p.strip.Match(files[p.file]) {
				files[p.file] = p.strip.ReplaceAll(files[p.file], nil)
				items = append(items, p.item)
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("check: %v (%s)", err, modulekit.Code(err))
		}
	}
}

func clone(files map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for k, v := range files {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// TestPackageSignsAndVerifies builds and signs the package with a throwaway key and
// checks it the way the core does at install. While the pinned core lacks a CORE item the
// package uses, it signs the package without that feature and skips at the end naming it.
func TestPackageSignsAndVerifies(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	all, err := quickbooks.Files()
	if err != nil {
		t.Fatal(err)
	}
	files, items := checked(t, all)
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
	if len(items) > 0 {
		tar, err := modulekit.Archive(files)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := modulekit.CheckPackage(tar); err != nil {
			t.Fatalf("check package: %v (%s)", err, modulekit.Code(err))
		}
		t.Skipf("the package as shipped needs %s; archived, signed and verified without those features", strings.Join(items, ", "))
	}

	// The package directory itself passes the install checks, read the way the CLI reads it.
	dir, err := modulekit.ReadDir("package")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := modulekit.Check(dir); err != nil {
		t.Fatalf("check: %v", err)
	}
	signed, err = quickbooks.Build(priv)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := modulekit.VerifyFile(signed, []modulekit.TrustedKey{modulekit.Trust(pub)}); err != nil {
		t.Fatalf("verify the shipped package: %v", err)
	}
	raw, err := quickbooks.Archive()
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := modulekit.CheckPackage(raw)
	if err != nil {
		t.Fatalf("check package: %v", err)
	}
	if pkg.Manifest.ID != quickbooks.ID {
		t.Fatalf("manifest id %q", pkg.Manifest.ID)
	}
}
