package mockbackend_test

import (
	"testing"

	"github.com/philsphicas/aztunnel/internal/testharness/e2escenarios"
	"github.com/philsphicas/aztunnel/mockrelay/testharness/mockbackend"
)

// BenchmarkE2E_Mock runs the shared e2e benchmark suite against
// the in-process MockBackend. Pair with `e2e/e2e_bench_test.go`
// (BenchmarkE2E_Azure, build tag e2e) for the source-of-truth
// numbers; the mock variant is the fast feedback loop for benchmark
// development.
//
// Standard invocation (-count=5 for stability, -benchmem for alloc
// numbers in benchstat output):
//
//	go test -run='^$' -bench=. -benchmem -count=5 \
//	    ./testharness/mockbackend/...
func BenchmarkE2E_Mock(b *testing.B) {
	var backend mockbackend.MockBackend
	e2escenarios.RunBenchmarks(b, &backend)
}
