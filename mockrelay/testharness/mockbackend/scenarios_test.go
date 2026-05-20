package mockbackend_test

import (
	"testing"

	"github.com/philsphicas/aztunnel/internal/testharness/e2escenarios"
	"github.com/philsphicas/aztunnel/mockrelay/testharness/mockbackend"
)

// TestE2E_Mock runs the shared e2e scenarios against the in-process
// MockBackend. This is the always-on side of the mock-vs-Azure
// conformance matrix; it runs in `go test ./mockrelay/...` and is what
// the CI `test` job enforces.
func TestE2E_Mock(t *testing.T) {
	var b mockbackend.MockBackend
	t.Run(b.Name(), func(t *testing.T) {
		e2escenarios.RunCoreScenarios(t, &b)
		e2escenarios.RunTopologyScenarios(t, &b)
		e2escenarios.RunReliabilityScenarios(t, &b)
		e2escenarios.RunObservabilityScenarios(t, &b)
	})
}
