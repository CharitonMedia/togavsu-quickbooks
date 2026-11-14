package quickbooks_test

import (
	"testing"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"
)

// TestSettingsSource runs the core's settings source check (AC-030) on the module root:
// no environment reads, and no numeric configuration literal outside a setting. Fixed
// numbers are spelled in each package's fixed.go, the convention of matter-clock.
func TestSettingsSource(t *testing.T) {
	if err := modulekit.CheckSource("."); err != nil {
		t.Fatal(err)
	}
}
