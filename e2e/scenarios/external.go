package scenarios

import (
	"os/exec"
	"testing"
)

// requireExternalTool skips the test if name is not present in PATH.
// Used by scenarios that shell out to an external binary that may not
// be installed in every environment (e.g. ssh on bare-bones CI
// containers). The skip reason names the tool so the operator knows
// what to install.
func requireExternalTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("external tool %q not in PATH: %v", name, err)
	}
}
