//go:build e2e

package e2e

import (
	"testing"

	"github.com/philsphicas/aztunnel/internal/testharness/e2escenarios"
)

// BenchmarkE2E_Azure runs the shared e2e benchmark suite
// against a real Azure Relay namespace. Pair with the mock variant
// (BenchmarkE2E_Mock in mockrelay/testharness/mockbackend) for fast
// iteration; the Azure variant produces source-of-truth numbers for
// characterising the relay's connect-latency and short-session-
// throughput dimensions on a real namespace.
//
// Only one auth method is exercised — whichever availableAuthNames
// returns first — to keep the run time bounded. Run with E2E_AUTH=sas
// or E2E_AUTH=entra to pin the method, which is important for
// benchstat name-matching across BASE and HEAD invocations of
// scripts/bench-compare.sh.
//
// All sub-benches share a single process-leased hyco pair
// (leaseSharedHyco, drained at TestMain exit). This is the only
// place in the suite where multiple Backend.Setup calls reuse the
// same pair: each sub-bench's listeners and senders are still tied
// to their own scenario t.Cleanup chain (so processes are reaped
// between sub-benches), but the ARM Provision cost is paid once
// for the whole benchmark run. This keeps -count=N benchstat
// invocations from amplifying the provisioning tail by N×.
//
// Standard invocation (each iteration involves real relay round-
// trips, so -benchtime=10x usually beats the default -benchtime=1s):
//
//	go test -tags=e2e -run='^$' -bench=. -benchmem -count=5 \
//	    -benchtime=10x -timeout=60m ./e2e/...
func BenchmarkE2E_Azure(b *testing.B) {
	requireProvider(b)
	name := availableAuthNames(b)[0]
	b.Run(name, func(b *testing.B) {
		backend := &azureBackend{authName: name, acquireEnv: leaseSharedHyco}
		e2escenarios.RunBenchmarks(b, backend)
	})
}
