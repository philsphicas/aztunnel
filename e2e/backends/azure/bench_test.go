//go:build e2e

package azure

import (
	"testing"

	"github.com/philsphicas/aztunnel/e2e/scenarios"
)

// BenchmarkE2E_Azure runs the shared e2e benchmark suite
// against a real Azure Relay namespace. Pair with the mock variant
// (BenchmarkE2E_Mock in e2e/backends/mock) for fast
// iteration; the Azure variant produces source-of-truth numbers for
// characterising the relay's connect-latency and short-session-
// throughput dimensions on a real namespace.
//
// Only one auth method is exercised — whichever availableAuthNames
// returns first — to keep the run time bounded. Run with E2E_AUTH=sas
// or E2E_AUTH=entra to pin the method, which is important for
// benchstat name-matching across BASE and HEAD invocations of the
// benchmark workflow. To exercise these from the repo root:
//
//	cd e2e && go test -tags=e2e -run='^$' -bench=. -benchmem \
//	  -count=1 -timeout=60m ./backends/azure/...
//
// (`make bench` runs only the mock-backend benchmarks; the Azure
// benchmarks require a real namespace and the invocation above.)
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
	f := &azureBackend{
		axis:       &authAxis{values: []string{name}},
		acquireEnv: leaseSharedHyco,
	}
	scenarios.RunAllBenchmarks(b, f)
}

// BenchmarkE2E_Azure_MuxMatrix runs the shared e2e benchmark suite
// twice — once with the sender's mux pool enabled (v2, the production
// default) and once with --no-mux (v1, the pre-mux behaviour) — by
// wrapping the backend with scenarios.WithMuxAxis. This is the
// Azure-side counterpart to BenchmarkE2E_Mock's matrix run; together
// they provide an apples-to-apples paired comparison that
// `benchstat -filter='.name:/v1/'` vs `-filter='.name:/v2/'` can
// summarise directly.
//
// Kept separate from BenchmarkE2E_Azure (which stays single-mode)
// because the matrix doubles total Azure benchmark wall-clock time
// and the CI bench workflow's default 60m timeout is already a tight
// fit for the single-mode run on -benchtime=10x -count=5. Operators
// who specifically want the mux numbers run this benchmark with a
// longer timeout or smaller -count; the steady-state CI workflow
// stays on BenchmarkE2E_Azure.
//
// Invocation:
//
//	go test -tags=e2e -run='^$' -bench=BenchmarkE2E_Azure_MuxMatrix \
//	    -benchmem -count=5 -benchtime=10x -timeout=120m ./e2e/...
func BenchmarkE2E_Azure_MuxMatrix(b *testing.B) {
	requireProvider(b)
	name := availableAuthNames(b)[0]
	f := &azureBackend{
		axis:       &authAxis{values: []string{name}},
		acquireEnv: leaseSharedHyco,
	}
	scenarios.RunAllBenchmarks(b, scenarios.WithMuxAxis(f))
}
