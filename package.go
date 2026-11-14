// Package quickbooks is the QuickBooks Sync module: a one-way push of the firm's money
// records from Togavsu into QuickBooks Online (docs/SPEC.md). It runs in its own
// container, reaches core data only through the core API and keeps its own tables in
// schema mod_quickbooks.
package quickbooks

import (
	"crypto/ed25519"
	"embed"
	"io/fs"
	"strings"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"
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

// Build signs the embedded package with priv into the signed package file the core
// installs.
func Build(priv ed25519.PrivateKey) ([]byte, error) {
	files, err := Files()
	if err != nil {
		return nil, err
	}
	return SignFile(priv, files)
}

// SignFile runs the core's install checks on package files and signs them with priv,
// through the core's module kit. It refuses to sign what the core would refuse.
func SignFile(priv ed25519.PrivateKey, files map[string][]byte) ([]byte, error) {
	if _, _, err := modulekit.Check(files); err != nil {
		return nil, err
	}
	return modulekit.Build(priv, files)
}

// Archive is the installable package (gzip tar) of the package files.
func Archive() ([]byte, error) {
	files, err := Files()
	if err != nil {
		return nil, err
	}
	return modulekit.Archive(files)
}
