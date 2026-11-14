// Package pendingcore lists, in one place, every part of the shipped package that the
// pinned core refuses until a module-core item lands, with the edit that removes it. The
// package test checks a copy with all of them stripped and asserts that the full package
// is refused for each one, so it fails once the core accepts a field and its entry can go.
// The acceptance suite installs the same stripped copy on the composed core.
package pendingcore

import "regexp"

// Manifest and the migration file the fields live in.
const (
	Manifest   = "togavsu-module.yaml"
	Connection = "migrations/0001_connection.sql"
)

// Field is one pending part of the package.
type Field struct {
	Item string // the CORE item that makes the core accept it
	Name string // what the field is
	File string // the package file that holds it
	// Refusal matches the install check's error for the field. A scope has none: the
	// install check passes it and the core's scope catalog refuses it at approval.
	Refusal *regexp.Regexp
	Scope   string // for a scope, its name
	strip   *regexp.Regexp
}

// Fields is the list.
var Fields = []Field{
	{Item: "CORE-03", Name: "notifications block", File: Manifest,
		Refusal: regexp.MustCompile(`unknown field "notifications"`),
		strip:   regexp.MustCompile(`(?m)^notifications:\n(?:  .*\n)+`)},
	{Item: "CORE-08", Name: "sidebar section, icon and order", File: Manifest,
		Refusal: regexp.MustCompile(`unknown field "(section|icon|order|subgroup|badge)"`),
		strip:   regexp.MustCompile(`(?m)^    (section|icon|order|subgroup|badge): .*\n`)},
	{Item: "CORE-22", Name: "trust_bank_map foreign key to core.trust_accounts", File: Connection,
		Refusal: regexp.MustCompile(`core\.trust_accounts is not a core record`),
		strip:   regexp.MustCompile(` REFERENCES core\.trust_accounts \(id\) ON DELETE RESTRICT`)},
	{Item: "CORE-02", Name: "scope books:read", File: Manifest, Scope: "books:read",
		strip: regexp.MustCompile(`(?m)^  - scope: books:read\n    reason: .*\n`)},
	{Item: "CORE-06", Name: "scope module_secrets:write", File: Manifest, Scope: "module_secrets:write",
		strip: regexp.MustCompile(`(?m)^  - scope: module_secrets:write\n    reason: .*\n`)},
}

// Present reports whether files still hold the field.
func (f Field) Present(files map[string][]byte) bool { return f.strip.Match(files[f.File]) }

// Strip returns a copy of files without the given fields.
func Strip(files map[string][]byte, fields ...Field) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for k, v := range files {
		out[k] = append([]byte(nil), v...)
	}
	for _, f := range fields {
		out[f.File] = f.strip.ReplaceAll(out[f.File], nil)
	}
	return out
}

// Checked are the fields the install check refuses; Scopes are the rest.
func Checked() (checked, scopes []Field) {
	for _, f := range Fields {
		if f.Refusal != nil {
			checked = append(checked, f)
		} else {
			scopes = append(scopes, f)
		}
	}
	return checked, scopes
}
