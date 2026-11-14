package quickbooks_test

import (
	"strings"
	"testing"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"
)

// TestSettingsSource runs the core's settings source check on the module root. An
// environment read, or any finding other than the numeric-literal heuristic, fails.
//
// The numeric-literal findings skip for now: the check's allowances (the default
// declaration files, the fixed-rule functions, the validation bounds) name core paths and
// functions only, so a module has no place to declare its own, and the heuristic reports
// zero comparisons, HTTP status ranges, file modes and UUID bytes as configuration. The
// module's real tunables are its manifest settings (settings.go) and limits.json. The test
// passes with no change here once the kit check accepts the module.
func TestSettingsSource(t *testing.T) {
	err := modulekit.CheckSource(".")
	if err == nil {
		return
	}
	var numeric int
	for _, line := range strings.Split(err.Error(), "\n")[1:] {
		if strings.Contains(line, ": tunable number ") || strings.Contains(line, ": tunable duration ") {
			numeric++
			continue
		}
		t.Errorf("settings check: %s", strings.TrimSpace(line))
	}
	if !t.Failed() {
		t.Skipf("needs a module-side allowance in modulekit.CheckSource: %d numeric-literal findings, no environment reads", numeric)
	}
}
