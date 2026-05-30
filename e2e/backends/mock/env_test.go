//go:build e2e

package mock_test

import (
	"os"
	"testing"

	"github.com/philsphicas/aztunnel/mockrelay/server"
)

// delayProfileFromEnv resolves the DelayProfile for the mock e2e
// scenario run from the E2E_DELAY environment variable, mirroring the
// E2E_AUTH knob the Azure backend uses to pin its auth axis.
//
// Unset (or empty) selects "default" — the wire-faithful profile the
// e2e timing thresholds are calibrated against — so the historical
// behaviour of TestE2E_Mock is unchanged. Any other value is looked up
// in the mockrelay profile registry (server.ProfileByName); an
// unrecognised name fails the test loudly with the list of known
// profiles, so a typo never silently falls through to the wrong timing
// model. Adding a profile to the registry makes it selectable here
// with no change to this helper.
func delayProfileFromEnv(t testing.TB) server.DelayProfile {
	t.Helper()
	name := os.Getenv("E2E_DELAY")
	if name == "" {
		name = "default"
	}
	p, err := server.ProfileByName(name)
	if err != nil {
		t.Fatalf("E2E_DELAY: %v", err)
	}
	return p
}
