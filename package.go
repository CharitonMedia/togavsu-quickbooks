// Package quickbooks is the QuickBooks Sync module: a one-way push of the firm's money
// records from Togavsu into QuickBooks Online (docs/SPEC.md). It runs in its own
// container, reaches core data only through the core API and keeps its own tables in
// schema mod_quickbooks.
package quickbooks

import (
	"crypto/ed25519"
	"embed"
	"errors"
	"io/fs"
	"strings"
)

// ID is the module id from the manifest.
const ID = "quickbooks"

//go:embed package
var packageFS embed.FS

//go:embed limits.json
var limitsJSON []byte

// Files returns the package files (manifest and migrations) by package path.
func Files() (map[string][]byte, error) {
	out := map[string][]byte{}
	err := fs.WalkDir(packageFS, "package", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := packageFS.ReadFile(p)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(p, "package/")] = b
		return nil
	})
	return out, err
}

// Packager turns the package files into the marketplace's signed package file. The
// archive format and the signature are the core's to define, so the module does not
// reimplement them: a Packager comes from the core's public signing package or CLI once
// it exists.
type Packager interface {
	Sign(files map[string][]byte, priv ed25519.PrivateKey) ([]byte, error)
}

// ErrPackagingUnavailable is what the stub packager answers.
var ErrPackagingUnavailable = errors.New("quickbooks: signing and packaging wait on the core's public signing package or CLI")

// StubPackager is the placeholder until the core publishes its signing path.
type StubPackager struct{}

// Sign always answers ErrPackagingUnavailable.
func (StubPackager) Sign(map[string][]byte, ed25519.PrivateKey) ([]byte, error) {
	return nil, ErrPackagingUnavailable
}

// Build signs the package with priv through p (StubPackager when p is nil).
func Build(p Packager, priv ed25519.PrivateKey) ([]byte, error) {
	if p == nil {
		p = StubPackager{}
	}
	files, err := Files()
	if err != nil {
		return nil, err
	}
	return p.Sign(files, priv)
}
