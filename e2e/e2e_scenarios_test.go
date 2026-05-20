//go:build e2e

package e2e

import (
	"testing"

	"github.com/philsphicas/aztunnel/internal/testharness/e2escenarios"
)

// TestE2E_Azure runs the shared core e2e scenario suite against a real
// Azure Relay namespace, once per available auth method (Entra, SAS).
// The same scenarios run against the in-process MockBackend in
// mockrelay/testharness/mockbackend — any divergence between this
// test and that one is a behavioural gap to fix in the mock.
//
// The auth dimension is declared on the backend as an axis
// (newAzureBackendFactory discovers it once via availableAuthNames at
// TestE2E_Azure entry); e2escenarios.RunAllScenarios enumerates it,
// wrapping each value in t.Run so the rendered sub-test paths read
// as TestE2E_Azure/<auth>/<scenario>.
//
// TestE2E_Azure does NOT call t.Parallel() because AssertNoLeaks
// (registered at the top of every scenario) samples process-wide
// goroutine + FD counts and would falsely fail if scenarios ran in
// parallel. Each scenario still gets isolation via per-Setup hyco
// provisioning inside azureBackend.Setup — provisioning runs serially
// at the e2e-suite level, which is fine because the Provider's
// concurrency cap is not the bottleneck here.
func TestE2E_Azure(t *testing.T) {
	requireProvider(t)
	f := newAzureBackendFactory(t, requireDedicatedHyco)
	e2escenarios.RunAllScenarios(t, f)
}
