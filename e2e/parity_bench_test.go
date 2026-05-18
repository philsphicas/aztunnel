//go:build e2e

package e2e

import (
	"testing"

	"github.com/philsphicas/aztunnel/internal/testharness/relayparity"
)

// BenchmarkParity_Azure runs the shared parity benchmark suite
// against a real Azure Relay namespace. Pair with the mock variant
// (BenchmarkParity_Mock in mockrelay/testharness/parity) for fast
// iteration; the Azure variant produces source-of-truth numbers for
// characterising PR #47.
//
// Only one auth method is exercised — whichever availableAuths
// returns first — to keep the run time bounded. Run with E2E_AUTH=sas
// or E2E_AUTH=entra to pin the method, which is important for
// benchstat name-matching across BASE and HEAD invocations of
// scripts/bench-compare.sh.
//
// Standard invocation (each iteration involves real relay round-
// trips, so -benchtime=10x usually beats the default -benchtime=1s):
//
//	go test -tags=e2e -run='^$' -bench=. -benchmem -count=5 \
//	    -benchtime=10x -timeout=60m ./e2e/...
func BenchmarkParity_Azure(b *testing.B) {
	env := requireRelayEnv(b)
	auths := availableAuths(b, env)
	auth := auths[0]
	b.Run(auth.name, func(b *testing.B) {
		backend := &azureBackend{env: env, auth: auth}
		relayparity.RunBenchSuite(b, backend)
	})
}
