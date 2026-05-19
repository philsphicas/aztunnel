//go:build e2e

package e2e

import (
	"testing"

	"github.com/philsphicas/aztunnel/internal/testharness/relayparity"
)

// TestParity_Azure runs the shared core parity suite against a real
// Azure Relay namespace, once per configured auth method (Entra,
// SAS). The same scenarios run against the in-process MockBackend in
// mockrelay/testharness/parity — any divergence between this test and that one is
// a behavioural gap to fix in the mock.
//
// TestParity_Azure does NOT call t.Parallel() because AssertNoLeaks
// (registered at the top of every scenario) samples process-wide
// goroutine + FD counts and would falsely fail if scenarios ran in
// parallel. Each scenario still gets isolation via per-Setup hyco
// provisioning inside azureBackend.Setup — provisioning runs serially
// at the parity-suite level, which is fine because the Provider's
// concurrency cap is not the bottleneck here.
func TestParity_Azure(t *testing.T) {
	requireProvider(t)
	for _, name := range availableAuthNames(t) {
		name := name
		t.Run(name, func(t *testing.T) {
			b := &azureBackend{authName: name, acquireEnv: requireDedicatedHyco}
			relayparity.RunCoreSuite(t, b)
			relayparity.RunTopologySuite(t, b)
			relayparity.RunReliabilitySuite(t, b)
			relayparity.RunObservabilitySuite(t, b)
		})
	}
}
