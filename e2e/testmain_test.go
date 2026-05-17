//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/e2e/azrelay"
)

// TestMain provisions per-invocation Azure Relay hybrid connections before
// any test runs, and tears them down on exit. Per-invocation isolation is
// what lets multiple CI pipelines (and concurrent local invocations) share
// a single relay namespace without cross-talk.
//
// The contract with individual tests is the existing E2E_* environment
// variables. helpers_test.go and every test read those via os.Getenv;
// TestMain sets them with os.Setenv after provisioning, so no test code
// needs to change.
//
// Required env vars for provisioning:
//
//   - E2E_RELAY_NAME       Azure Relay namespace name
//   - E2E_RESOURCE_GROUP   Resource group containing the namespace
//   - AZURE_SUBSCRIPTION_ID Subscription ID for ARM API calls
//
// If E2E_RELAY_NAME is empty, provisioning is skipped and tests fall
// through to their existing t.Skip behavior in requireRelayEnv. This
// preserves the historic ergonomics for contributors running `go test`
// without an Azure account.
func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	if os.Getenv("E2E_RELAY_NAME") == "" {
		// No relay configured. Surface this prominently so a developer
		// running `make e2e` without first sourcing the env vars does
		// not mistake an all-skipped run for an all-passing run.
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

	prov, err := azrelay.New(azrelay.Config{
		SubscriptionID: sub,
		ResourceGroup:  rg,
		Namespace:      os.Getenv("E2E_RELAY_NAME"),
	})
	if err != nil {
		fatal("azrelay.New: %v", err)
	}

	entra, sas := prov.HycoNames()
	fmt.Fprintf(os.Stderr, "==> provisioning hybrid connections in %s/%s: %s, %s\n", rg, os.Getenv("E2E_RELAY_NAME"), entra, sas)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	result, err := prov.Provision(ctx)
	cancel()
	if err != nil {
		fatal("provision: %v", err)
	}

	// Export the test contract. We unconditionally overwrite any existing
	// value because TestMain's per-invocation hycos take precedence over
	// any stale env vars the caller may have set from previous runs.
	for k, v := range result.EnvVars() {
		if err := os.Setenv(k, v); err != nil {
			fatal("setenv %s: %v", k, err)
		}
	}

	code := m.Run()

	teardownCtx, teardownCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer teardownCancel()
	if err := prov.Teardown(teardownCtx); err != nil {
		// Log but do not fail the test run on teardown errors — the
		// janitor workflow will clean up anything we miss.
		fmt.Fprintf(os.Stderr, "warning: teardown: %v\n", err)
	}

	return code
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "TestMain: "+format+"\n", args...)
	os.Exit(1)
}
