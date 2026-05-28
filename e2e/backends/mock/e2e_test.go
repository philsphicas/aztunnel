//go:build e2e

package mock_test

import (
	"testing"

	"github.com/philsphicas/aztunnel/e2e/backends/mock"
	"github.com/philsphicas/aztunnel/e2e/scenarios"
	"github.com/philsphicas/aztunnel/mockrelay/server"
)

// TestE2E_Mock runs the shared e2e scenarios against the in-process
// MockBackend. This is the always-on side of the mock-vs-Azure
// conformance matrix; it runs in `make e2e-mock` and pairs with
// TestE2E_Azure in e2e/backends/azure to surface any behavioural
// divergence between the mock and Azure.
//
// DelayProfileDefault is the recommended profile for e2e-style runs:
// it approximates the wireshark-observed wall-clock shape from real
// Azure Relay captures so timing thresholds calibrated against Azure
// also fire against the mock.
func TestE2E_Mock(t *testing.T) {
	b := mock.MockBackend{DelayProfile: server.DelayProfileDefault}
	scenarios.RunAllScenarios(t, &b)
}
