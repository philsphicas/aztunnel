package parity_test

import (
	"testing"

	"github.com/philsphicas/aztunnel/internal/relayparity"
	"github.com/philsphicas/aztunnel/mockrelay/parity"
)

// TestParity_Mock runs the shared parity suites against the in-process
// MockBackend. This is the always-on side of the parity matrix; it
// runs in `go test ./mockrelay/...` and is what the CI `test` job
// enforces.
func TestParity_Mock(t *testing.T) {
	var b parity.MockBackend
	t.Run(b.Name(), func(t *testing.T) {
		relayparity.RunCoreSuite(t, &b)
		relayparity.RunTopologySuite(t, &b)
	})
}
