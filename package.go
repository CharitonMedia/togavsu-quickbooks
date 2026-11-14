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

// Files returns the package files (manifest, migrations and pending.yaml) by package path,
// as written. Installable is what ships.
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

// Installable is the package the pinned core accepts: the files without pending.yaml and
// without each part it lists that the core still refuses (modulekit.Installable), with
// the parts it held back and the entries that held nothing back. A refusal pending.yaml
// doesn't cover is an error.
func Installable() (modulekit.InstallablePackage, error) {
	files, err := Files()
	if err != nil {
		return modulekit.InstallablePackage{}, err
	}
	return modulekit.Installable(files)
}

// Pending is the pending list (pending.yaml) as written.
func Pending() ([]modulekit.Pending, error) {
	files, err := Files()
	if err != nil {
		return nil, err
	}
	return modulekit.ParsePending(files[modulekit.PendingFile])
}

// Build signs the installable package with priv into the signed package file the core
// installs (modulekit.BuildInstallable), and returns Installable's result with it.
func Build(priv ed25519.PrivateKey) ([]byte, modulekit.InstallablePackage, error) {
	files, err := Files()
	if err != nil {
		return nil, modulekit.InstallablePackage{}, err
	}
	return modulekit.BuildInstallable(priv, files)
}

// Archive is the installable package (gzip tar), without the held-back parts.
func Archive() ([]byte, error) {
	ip, err := Installable()
	if err != nil {
		return nil, err
	}
	return modulekit.Archive(ip.Files)
}
