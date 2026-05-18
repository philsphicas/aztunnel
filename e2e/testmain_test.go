//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/e2e/azrelay"
)

// relayProvider is the process-scoped factory used by
// requireDedicatedHyco to hand out fresh per-test hyco pairs. nil
// when E2E_RELAY_NAME is unset, in which case every helper that
// requires Azure routes to t.Skip via requireProvider.
//
// Lifecycle: constructed once at TestMain entry, no explicit
// teardown. The Provider holds only an *armrelay.HybridConnectionsClient
// (GC-safe) and a buffered channel (GC-safe). Individual PairTokens
// it hands out own their own Teardown via t.Cleanup in
// requireDedicatedHyco.
var relayProvider *azrelay.Provider

// sharedHycoToken keeps the legacy pre-provisioned pair alive for
// the duration of the run so tests that have not yet been migrated
// to requireDedicatedHyco can still read the E2E_* environment
// variables and use the same pair across the suite. Phase 3 of the
// per-test-isolation work will remove this pre-provisioning along
// with the legacy env-var reads in requireRelayEnv.
var sharedHycoToken *azrelay.PairToken

// TestMain prepares the e2e suite by constructing the relay Provider
// (which fronts a configurable-concurrency semaphore for per-test
// provisioning) and provisioning one shared hyco pair for legacy
// callers. The shared pair's connection metadata is exported through
// the existing E2E_* environment variables; the per-test Provider is
// reached via requireDedicatedHyco / requireProvider helpers.
//
// Required env vars:
//
//   - E2E_RELAY_NAME            Azure Relay namespace name
//   - E2E_RESOURCE_GROUP        Resource group containing the namespace
//   - AZURE_SUBSCRIPTION_ID     Subscription ID for ARM API calls
//
// Optional env var:
//
//   - E2E_PROVISIONER_CONCURRENCY  cap on in-flight Provision calls
//     across all t.Parallel test goroutines (default 4). Raise only
//     when CI data shows headroom under the namespace-level 429
//     envelope.
//
// If E2E_RELAY_NAME is unset, provisioning is skipped and tests fall
// through to t.Skip via requireRelayEnv / requireProvider. This
// preserves the historic ergonomics for contributors running
// `go test` without an Azure account.
func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	if os.Getenv("E2E_RELAY_NAME") == "" {
		fmt.Fprintln(os.Stderr, "==> e2e: E2E_RELAY_NAME is unset — TestMain will not provision hybrid connections")
		fmt.Fprintln(os.Stderr, "==> e2e: every test will be SKIPPED. Run `eval \"$(make e2e-infra-env)\"` first, or set E2E_RELAY_NAME explicitly.")
		return m.Run()
	}

	sub := os.Getenv("AZURE_SUBSCRIPTION_ID")
	rg := os.Getenv("E2E_RESOURCE_GROUP")
	if sub == "" || rg == "" {
		fatal("E2E_RELAY_NAME is set but AZURE_SUBSCRIPTION_ID and/or E2E_RESOURCE_GROUP are missing\n" +
			"  set AZURE_SUBSCRIPTION_ID and E2E_RESOURCE_GROUP, or unset E2E_RELAY_NAME to skip e2e tests")
	}

	conc := readConcurrencyEnv()

	p, err := azrelay.NewProvider(azrelay.Config{
		SubscriptionID: sub,
		ResourceGroup:  rg,
		Namespace:      os.Getenv("E2E_RELAY_NAME"),
		Concurrency:    conc,
	})
	if err != nil {
		fatal("azrelay.NewProvider: %v", err)
	}
	relayProvider = p

	// Phase-2 transitional: provision one shared hyco pair so tests
	// that still call requireRelayEnv (i.e. have not migrated to
	// requireDedicatedHyco) continue to read working E2E_* env vars.
	// Phase 3 removes both this block and the legacy env-var path.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	tok, err := relayProvider.Provision(ctx)
	cancel()
	if err != nil {
		fatal("provision shared hyco pair: %v", err)
	}
	sharedHycoToken = tok
	entra, sas := tok.HycoNames()
	fmt.Fprintf(os.Stderr, "==> shared hyco pair provisioned in %s/%s: %s, %s (concurrency=%d)\n",
		rg, os.Getenv("E2E_RELAY_NAME"), entra, sas, conc)
	for k, v := range tok.Result().EnvVars() {
		if err := os.Setenv(k, v); err != nil {
			fatal("setenv %s: %v", k, err)
		}
	}

	code := m.Run()

	teardownCtx, teardownCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer teardownCancel()
	if err := sharedHycoToken.Teardown(teardownCtx); err != nil {
		// Log but do not fail the run on teardown errors — the
		// janitor workflow will reap anything we miss.
		fmt.Fprintf(os.Stderr, "warning: shared hyco teardown: %v\n", err)
	}

	return code
}

// readConcurrencyEnv parses E2E_PROVISIONER_CONCURRENCY. Anything
// invalid or non-positive falls back to azrelay's default so a
// typo cannot accidentally serialise or stampede the provisioner.
func readConcurrencyEnv() int {
	raw := os.Getenv("E2E_PROVISIONER_CONCURRENCY")
	if raw == "" {
		return azrelay.DefaultProvisionerConcurrency
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		fmt.Fprintf(os.Stderr, "==> e2e: ignoring invalid E2E_PROVISIONER_CONCURRENCY=%q (using default %d)\n",
			raw, azrelay.DefaultProvisionerConcurrency)
		return azrelay.DefaultProvisionerConcurrency
	}
	return n
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "TestMain: "+format+"\n", args...)
	os.Exit(1)
}
