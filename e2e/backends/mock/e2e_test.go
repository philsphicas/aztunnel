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
func TestE2E_Mock(t *testing.T) {
	var b mock.MockBackend
	scenarios.RunAllScenarios(t, &b)
}
