//go:build e2e

package mock_test

import (
	"testing"

	"github.com/philsphicas/aztunnel/e2e/scenarios"
)

// TestE2E_Mock runs the shared e2e scenarios against the in-process
// MockBackend. This is the always-on side of the mock-vs-Azure
// conformance matrix; it runs in `make e2e-mock` and pairs with
// TestE2E_Azure in e2e/backends/azure to surface any behavioural
// divergence between the mock and Azure.
//
// The backend is built via mockBackendFromEnv, which composes two
// independent, env-driven dimensions into one matrix:
//
//   - E2E_AUTH selects the auth method(s): unset runs both {sas, entra}
//     (mirroring the Azure backend), or pin one with E2E_AUTH=sas /
//     E2E_AUTH=entra. The entra cell models the client-side
//     token-acquisition cost (a one-off cold AAD round trip, cached
//     thereafter) that real Entra auth pays and SAS does not — its size
//     comes from the selected delay profile's TokenAcquire.
//   - E2E_DELAY selects the delay profile(s): unset runs the
//     wire-faithful "default" profile (whose wall-clock approximates
//     wireshark-observed real Azure Relay captures, so timing
//     thresholds calibrated against Azure also fire against the mock),
//     "all" runs every registered profile, or pass a comma-separated
//     list. The "zero" profile disables all synthetic delay, including
//     the entra token-acquisition cost, for a fast smoke run.
//
// A dimension with a single value adds no sub-test layer; with two or
// more it does (auth outermost, then delay). See env_test.go and the
// mockrelay profile registry for details.
func TestE2E_Mock(t *testing.T) {
	scenarios.RunAllScenarios(t, mockBackendFromEnv(t))
}
