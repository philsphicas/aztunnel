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
// (GC-safe), a buffered channel (GC-safe), and a reference to the
// process-scoped *azrelay.RunRules (whose lifetime is owned by
// TestMain — see runRules below). Individual PairTokens it hands out
// own their own Teardown via t.Cleanup in requireDedicatedHyco.
var relayProvider *azrelay.Provider

// runRules holds the two namespace-scoped SAS authorization rules
// (Listen-only + Send-only) provisioned once at TestMain startup and
// shared by every PairToken's Result. Teardown is deferred from
// testMain so the rules are released on every normal exit path; the
// janitor reaps anything we leak (e.g. on os.Exit before defers).
var runRules *azrelay.RunRules

// runRuleAcquireTimeout bounds the namespace-rule provisioning step
// in TestMain. Two CreateOrUpdateAuthorizationRule + two ListKeys
// + the 2 s data-plane settle; with the SDK's 6-retry tail factored
// in, the worst-case observed wall-clock is well under a minute. 90 s
// gives comfortable headroom for a regional blip without letting a
// stuck control plane hang the suite.
const runRuleAcquireTimeout = 90 * time.Second

// runRuleTeardownTimeout bounds the deferred Teardown call. Two
// DeleteAuthorizationRule round-trips against the namespace.
const runRuleTeardownTimeout = 60 * time.Second

// TestMain constructs the process-scoped relay Provider so every
// test can call requireDedicatedHyco to provision its own private
// hyco pair. There is no shared pre-provisioned pair — each test
// owns its hyco lifetime end-to-end, which lets the suite run with
// t.Parallel().
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
// If E2E_RELAY_NAME is unset, the Provider is not constructed and
// every test falls through to t.Skip via requireProvider. This
// preserves the historic ergonomics for contributors running
// `go test` without an Azure account.
func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	// Pre-build cmd/aztunnel before any test runs. The build is
	// reused via sync.Once, but evaluating it lazily inside a test
	// races against any per-test context deadline the test has
	// already set on its exec.CommandContext (e.g. the 5s budget in
	// TestMissingRequiredArgs). On a cold CI machine the cmd/aztunnel
	// build is well over that budget, so the first test to call
	// aztunnelBinary would see its exec immediately fail with a
	// deadline-exceeded error and empty stderr. Pay the build cost
	// once, here, before m.Run().
	if err := buildAztunnelBinary(); err != nil {
		fatal("pre-build aztunnel: %v", err)
	}

	if os.Getenv("E2E_RELAY_NAME") == "" {
		fmt.Fprintln(os.Stderr, "==> e2e: E2E_RELAY_NAME is unset — TestMain will not construct a Provider")
		fmt.Fprintln(os.Stderr, "==> e2e: every Azure-dependent test will be SKIPPED (CLI-only tests still run). Run `eval \"$(make e2e-infra-env)\"` first, or set E2E_RELAY_NAME explicitly.")
		// drainBenchLease is a no-op when no benchmark could lease;
		// no Provider was constructed so leaseSharedHyco cannot have
		// run. No need to defer it on this skip path.
		return m.Run()
	}

	sub := os.Getenv("AZURE_SUBSCRIPTION_ID")
	rg := os.Getenv("E2E_RESOURCE_GROUP")
	if sub == "" || rg == "" {
		fatal("E2E_RELAY_NAME is set but AZURE_SUBSCRIPTION_ID and/or E2E_RESOURCE_GROUP are missing\n" +
			"  set AZURE_SUBSCRIPTION_ID and E2E_RESOURCE_GROUP, or unset E2E_RELAY_NAME to skip e2e tests")
	}

	conc := readConcurrencyEnv()

	cfg := azrelay.Config{
		SubscriptionID: sub,
		ResourceGroup:  rg,
		Namespace:      os.Getenv("E2E_RELAY_NAME"),
		Concurrency:    conc,
	}

	// Acquire the run-scoped namespace SAS rules. Two rules
	// (e2e-run-<hex>-listener with Listen, e2e-run-<hex>-sender with
	// Send) are created once here and stamped onto every PairToken
	// Result by Provider.Provision; per-test provisioning no longer
	// touches authorizationRules. Failures here are fatal — without
	// the rules the SAS auth path can't function. No defers earlier
	// than this point need to skip on the fatal path because nothing
	// has been provisioned yet.
	acquireCtx, acquireCancel := context.WithTimeout(context.Background(), runRuleAcquireTimeout)
	rr, err := azrelay.AcquireRunRules(acquireCtx, cfg)
	acquireCancel()
	if err != nil {
		fatal("azrelay.AcquireRunRules: %v", err)
	}
	runRules = rr
	fmt.Fprintf(os.Stderr, "==> e2e: run rules ready (listener=%s, sender=%s)\n",
		rr.ListenerName, rr.SenderName)

	// Defer rule teardown FIRST so it runs LAST on the LIFO stack —
	// every subsequently-deferred cleanup (e.g. drainBenchLease,
	// below) gets to use the SAS rules while it still tears down
	// in-flight state. The janitor reaps anything we leak (os.Exit
	// paths skip defers).
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), runRuleTeardownTimeout)
		defer cancel()
		if err := rr.Teardown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "==> e2e: teardown run rules %s/%s: %v\n",
				rr.ListenerName, rr.SenderName, err)
		}
	}()

	// Drain the shared bench hyco lease on every exit path, including
	// panics that the testing framework recovers from. Registered
	// AFTER the run-rule teardown defer so it runs BEFORE rule
	// teardown (Go defers are LIFO) — keeps the SAS rules alive for
	// any hyco-cleanup-time data-plane work the benchmark teardown
	// path may grow in the future. Today tok.Teardown is ARM-only,
	// so the order is currently not load-bearing.
	defer drainBenchLease()

	cfg.RunRules = rr
	p, err := azrelay.NewProvider(cfg)
	if err != nil {
		// Non-fatal: report and return non-zero so the deferred
		// teardown runs. fatal() (os.Exit) would skip it.
		fmt.Fprintf(os.Stderr, "TestMain: azrelay.NewProvider: %v\n", err)
		return 1
	}
	relayProvider = p
	fmt.Fprintf(os.Stderr, "==> e2e: relay Provider ready (namespace=%s/%s, concurrency=%d)\n",
		rg, os.Getenv("E2E_RELAY_NAME"), conc)

	return m.Run()
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
