//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/philsphicas/aztunnel/e2e/azrelay"
)

// relayProvider is the process-scoped factory used by
// requireDedicatedHyco to hand out fresh per-test hyco pairs.
// requireProvider handles the nil case defensively, but TestMain
// exits non-zero before tests run if config cannot be resolved.
//
// Lifecycle: constructed once at TestMain entry, no explicit
// teardown. The Provider holds only an *armrelay.HybridConnectionsClient
// (GC-safe), a buffered channel (GC-safe), and a reference to the
// process-scoped *azrelay.RunRules (whose lifetime is owned by
// TestMain — see runRules below). Individual PairTokens it hands out
// own their own Teardown via t.Cleanup in requireDedicatedHyco.
var relayProvider *azrelay.Provider

// runRules holds the two permanent namespace-scoped SAS authorization
// rules (Listen-only + Send-only) provisioned out-of-band by
// `make e2e-setup`. The keys are read once at TestMain startup and
// shared by every PairToken's Result. There is no per-run teardown —
// the rules outlive every test invocation.
var runRules *azrelay.RunRules

// runRuleAcquireTimeout bounds the namespace-rule key-fetch step in
// TestMain. Two ListKeys round-trips against permanent rules. 90 s
// gives comfortable headroom for a regional ARM blip without letting a
// stuck control plane hang the suite.
const runRuleAcquireTimeout = 90 * time.Second

const noConfigMessage = "==> e2e: no configuration found.\n" +
	"    Run `make e2e-setup` to provision a per-developer Azure Relay namespace,\n" +
	"    or `make e2e-attach RESOURCE_GROUP=<rg>` to record a pre-existing one."

// TestMain constructs the process-scoped relay Provider so every
// test can call requireDedicatedHyco to provision its own private
// hyco pair. There is no shared pre-provisioned pair — each test
// owns its hyco lifetime end-to-end, which lets the suite run with
// t.Parallel().
//
// Config resolution order:
//
//  1. AZURE_SUBSCRIPTION_ID + E2E_RESOURCE_GROUP env vars are both set:
//     use env vars (CI / explicit override), with E2E_RELAY_NAME read
//     from env as well.
//  2. Else read e2e/.local.json from the package cwd (`./.local.json`).
//  3. Else fail with a directive error that points to `make e2e-setup`
//     or `make e2e-attach`.
//
// Optional env var:
//
//   - E2E_PROVISIONER_CONCURRENCY  cap on in-flight Provision calls
//     across all t.Parallel test goroutines (default 4). Raise only
//     when CI data shows headroom under the namespace-level 429
//     envelope.
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

	resolved, err := resolveTestConfig(os.LookupEnv, ".")
	if err != nil {
		if errors.Is(err, errNoConfig) {
			fatal("%s", noConfigMessage)
		}
		fatal("%v", err)
	}
	if resolved.RelayName == "" {
		fatal("e2e config is missing relayName; set E2E_RELAY_NAME for env-based config or re-run `make e2e-setup`")
	}

	if resolved.Source == "local-config" {
		currentOID := func(ctx context.Context) (string, error) {
			cred, err := azidentity.NewDefaultAzureCredential(nil)
			if err != nil {
				return "", fmt.Errorf("default azure credential: %w", err)
			}
			tk, err := cred.GetToken(ctx, policy.TokenRequestOptions{
				Scopes: []string{"https://management.azure.com/.default"},
			})
			if err != nil {
				return "", fmt.Errorf("acquire ARM token: %w", err)
			}
			oid, err := parseOIDFromJWT(tk.Token)
			if err != nil {
				return "", fmt.Errorf("parse oid from ARM token: %w", err)
			}
			return oid, nil
		}
		identityCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = checkIdentityDrift(identityCtx, resolved.Local, currentOID)
		cancel()
		if err != nil {
			fatal("%v", err)
		}
		if resolved.Local != nil && resolved.Local.SASOnly {
			if _, isSet := os.LookupEnv("E2E_AUTH"); !isSet {
				if err := os.Setenv("E2E_AUTH", "sas"); err != nil {
					fatal("set E2E_AUTH=sas: %v", err)
				}
				fmt.Fprintln(os.Stderr, "==> e2e: SAS-only mode per e2e/.local.json. Entra subtests will skip.")
				fmt.Fprintln(os.Stderr, "    After someone grants you the Azure Relay role, run `make e2e-attach RESOURCE_GROUP=… RELAY_NAME=…` to refresh.")
			}
		}
	}

	conc := readConcurrencyEnv()

	// SAS-only mode signal: when the test process will only exercise
	// SAS, skip provisioning an entra hyco. E2E_AUTH is authoritative
	// — an explicit E2E_AUTH=entra (e.g. the user was granted the
	// role but hasn't refreshed .local.json yet) keeps entra
	// provisioning enabled even when SASOnly is persisted. When
	// E2E_AUTH is unset, fall back to resolved.Local.SASOnly so a
	// future contributor deleting the setenv block above does not
	// silently regress SkipEntra. (When SASOnly is true and E2E_AUTH
	// was unset on entry, the setenv block above has already forced
	// E2E_AUTH=sas, so this fallback only fires when both signals
	// would have produced the same answer.)
	skipEntra := false
	v, isSet := os.LookupEnv("E2E_AUTH")
	switch {
	case isSet && v == "sas":
		skipEntra = true
	case !isSet:
		if resolved.Local != nil && resolved.Local.SASOnly {
			skipEntra = true
		}
	}

	cfg := azrelay.Config{
		SubscriptionID: resolved.Subscription,
		ResourceGroup:  resolved.ResourceGroup,
		Namespace:      resolved.RelayName,
		Concurrency:    conc,
		SkipEntra:      skipEntra,
	}

	// Acquire the namespace SAS rule keys. The rules
	// (e2e-listener with Listen, e2e-sender with Send) are permanent
	// fixtures of the namespace provisioned once by `make e2e-setup`;
	// AcquireRunRules only reads their ListKeys output and never
	// creates or deletes rules. Hyco provisioning never mutates
	// authorization rules either. Failures here are fatal — without
	// the keys the SAS auth path can't function. No defers earlier
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

	// Drain the shared bench hyco lease on every exit path, including
	// panics that the testing framework recovers from. The permanent
	// SAS rules are not torn down here (they outlive the test
	// invocation), so this is the only TestMain-level cleanup defer.
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
		resolved.ResourceGroup, resolved.RelayName, conc)

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
