//go:build windows

package policy

import (
	"strings"
	"testing"
)

func TestUninstallFiltersValuesByExtensionID(t *testing.T) {
	script := uninstallScript("own-id", []string{"Chrome"})
	if !strings.Contains(script, "own-id;*") || !strings.Contains(script, "$key.GetValue($name)") {
		t.Fatalf("unfiltered uninstall script: %s", script)
	}
	if strings.Contains(script, "-Name '1'") || strings.Contains(script, forcelistKeys["Edge"]) {
		t.Fatalf("unrelated policy could be removed: %s", script)
	}
}
