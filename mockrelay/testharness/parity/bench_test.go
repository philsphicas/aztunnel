package parity_test

import (
	"testing"

	"github.com/philsphicas/aztunnel/internal/testharness/relayparity"
	"github.com/philsphicas/aztunnel/mockrelay/testharness/parity"
)

// BenchmarkParity_Mock runs the shared parity benchmark suite against
// the in-process MockBackend. Pair with `e2e/parity_bench_test.go`
// (BenchmarkParity_Azure, build tag e2e) for the source-of-truth
// numbers; the mock variant is the fast feedback loop for benchmark
// development.
//
// Standard invocation (-count=5 for stability, -benchmem for alloc
// numbers in benchstat output):
//
//	go test -run='^$' -bench=. -benchmem -count=5 \
//	    ./testharness/parity/...
func BenchmarkParity_Mock(b *testing.B) {
	var backend parity.MockBackend
	relayparity.RunBenchSuite(b, &backend)
}
