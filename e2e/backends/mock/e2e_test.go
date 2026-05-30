//go:build e2e

package mock_test

import (
	"testing"

	"github.com/philsphicas/aztunnel/e2e/backends/mock"
	"github.com/philsphicas/aztunnel/e2e/scenarios"
)

// TestE2E_Mock runs the shared e2e scenarios against the in-process
// MockBackend. This is the always-on side of the mock-vs-Azure
// conformance matrix; it runs in `make e2e-mock` and pairs with
// TestE2E_Azure in e2e/backends/azure to surface any behavioural
// divergence between the mock and Azure.
//
// The backend is built via NewAuthAxisBackend so the suite runs once
// per auth method (sas, entra), mirroring the Azure backend's matrix.
// The entra cell models the client-side token-acquisition cost (a
// one-off cold AAD round trip, cached thereafter) that real Entra auth
// pays and SAS does not.
//
// DelayProfileDefault is the recommended profile for e2e-style runs:
// it approximates the wireshark-observed wall-clock shape from real
// Azure Relay captures so timing thresholds calibrated against Azure
// also fire against the mock. Override the profile for a single run
// with the E2E_DELAY environment variable (e.g. `E2E_DELAY=zero`); see
// delayProfileFromEnv and the mockrelay profile registry for the set
// of selectable names.
func TestE2E_Mock(t *testing.T) {
	b := mock.NewAuthAxisBackend(delayProfileFromEnv(t))
	scenarios.RunAllScenarios(t, b)
}
