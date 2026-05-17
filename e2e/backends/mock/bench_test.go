//go:build e2e

package mock_test

import (
	"testing"

	"github.com/philsphicas/aztunnel/e2e/backends/mock"
	"github.com/philsphicas/aztunnel/e2e/scenarios"
)

// BenchmarkE2E_Mock runs the shared e2e benchmark suite against
// the in-process MockBackend, automatically enumerating both the
// v1 (mux-disabled) and v2 (mux-enabled) sender paths via the
// scenarios.WithMuxAxis decorator. Pair with BenchmarkE2E_Azure in
// e2e/backends/azure for the source-of-truth numbers; the mock
// variant is the fast feedback loop for benchmark development.
//
// Standard invocation (-count=5 for stability, -benchmem for alloc
// numbers in benchstat output):
//
//	go test -tags=e2e -run='^$' -bench=. -benchmem -count=5 \
//	    ./e2e/backends/mock/...
//
// The sub-bench paths are shaped `.../v1/<bench>` and `.../v2/<bench>`
// so `benchstat` can pair v1 vs v2 with a trivial filter:
//
//	go test ... -bench=. -count=10 | tee mock.txt
//	benchstat -filter='.name:/v1/' mock.txt -filter='.name:/v2/' mock.txt
func BenchmarkE2E_Mock(b *testing.B) {
	var backend mock.MockBackend
	scenarios.RunAllBenchmarks(b, scenarios.WithMuxAxis(&backend))
}
