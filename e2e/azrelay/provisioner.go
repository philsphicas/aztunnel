// Package azrelay provisions ephemeral Azure Relay hybrid connections for
// end-to-end tests.
//
// Each test process creates its own pair of hybrid connections — one for
// Entra ID authentication and one for SAS key authentication — at startup
// and tears them down at shutdown. This isolates concurrent test runs from
// each other: two pipelines using the same relay namespace cannot route
// flows to each other's listeners because they hold disjoint hyco names.
//
// The package depends only on the Azure SDK for Go and is safe to import
// from non-test code; the actual test wiring lives in e2e/testmain_test.go.
//
// Naming: hycos are named e2e-entra-<suffix> and e2e-sas-<suffix> where
// <suffix> is a 12-character random hex string. The pattern is matched by
// the janitor (e2e/infra/janitor) to identify orphaned hycos left behind
// by killed runners.
package azrelay

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/relay/armrelay"
)

// HycoNamePattern matches per-invocation hybrid connection names created by
// this package. The janitor uses this exact pattern (anchored) to identify
// orphaned hycos that should be cleaned up.
var HycoNamePattern = regexp.MustCompile(`^e2e-(entra|sas)-[0-9a-f]{12}$`)

// suffixLen is the length in hex characters of the random suffix appended
// to hyco names. 12 hex chars = 48 bits of entropy ≈ 2.8 × 10^14 possible
// values — collision probability between two simultaneous runs is negligible
// for any realistic CI volume.
const suffixLen = 12

// Bounds on the createRule / bestEffortDelete retry loop that absorbs
// the transient 40901 MessagingGatewayTooManyRequests conflict produced
// when Azure Relay's control plane serialises authorizationRule
// mutations per scope: the ARM SDK returns 2xx for one mutation before
// the backend has committed it, so a back-to-back mutation on the same
// scope (e.g. listener-rule then sender-rule create at namespace scope,
// or delete-during-pending-create) can race the per-scope serialization
// gate and get rejected with 40901. This is the same constraint that
// requires `@batchSize(1)` in Bicep on Microsoft.Relay/.../authorizationRules.
//
// Six attempts with equal-jittered exponential backoff (500ms → 8s, cap)
// is comfortably more than empirically observed convergence (sub-second)
// while still bounded so a genuinely stuck control plane fails the run
// rather than hanging. Note: the underlying azcore HTTP pipeline already
// retries 429s a small number of times honouring Retry-After before
// surfacing the error here, so the real cumulative wall time of a fully
// exhausted createRule call is larger than ~15.5s of sleep alone.
const (
	authRuleMaxAttempts  = 6
	authRuleInitialDelay = 500 * time.Millisecond
	authRuleMaxDelay     = 8 * time.Second
)

// Config describes which Azure subscription / resource group / namespace
// the provisioner should create hycos in. All fields are required.
type Config struct {
	SubscriptionID string
	ResourceGroup  string
	Namespace      string

	// Cred is the Azure credential used for ARM calls. If nil, Provision
	// will construct a DefaultAzureCredential.
	Cred azcore.TokenCredential

	// ClientOptions is forwarded to the armrelay clients. Nil applies
	// the per-test-tuned retry policy returned by DefaultClientOptions
	// (azcore MaxRetries=DefaultARMMaxRetries, honouring Retry-After
	// headers up to DefaultARMMaxRetryDelay).
	ClientOptions *arm.ClientOptions

	// Concurrency caps the number of in-flight Provider.Provision
	// calls. Zero applies DefaultProvisionerConcurrency. Ignored by
	// the single-use Provisioner type (it serialises by construction).
	Concurrency int

	// SkipEntra suppresses creation of the Entra-auth hyco. When true,
	// Provision creates only the SAS hyco and leaves
	// Result.EntraHycoName empty; Teardown skips the Delete on the
	// empty name. Used by the e2e test harness when running in
	// SAS-only mode (no Azure Relay data-plane role assignment), where
	// the entra hyco would be created and torn down only to never be
	// used. Safe to leave false in CI and any other path that
	// exercises both auth methods.
	SkipEntra bool

	// RunRules supplies the namespace-permanent SAS rules whose keys
	// every PairToken Result stamps onto its ListenerKey/SenderKey
	// fields. Required for NewProvider and Provisioner; AcquireRunRules
	// populates it.
	RunRules *RunRules
}

// Result holds the data needed by tests to connect to the freshly-created
// hybrid connections. Mirrors the existing E2E_* environment-variable
// contract documented in e2e/README.md.
type Result struct {
	RelayName       string
	EntraHycoName   string
	SASHycoName     string
	ListenerKeyName string
	ListenerKey     string
	SenderKeyName   string
	SenderKey       string
}

// EnvVars returns the Result formatted as the existing E2E_* environment
// variables. Callers (e.g. TestMain) typically pass each entry to os.Setenv.
func (r *Result) EnvVars() map[string]string {
	return map[string]string{
		"E2E_RELAY_NAME":            r.RelayName,
		"E2E_ENTRA_HYCO_NAME":       r.EntraHycoName,
		"E2E_SAS_HYCO_NAME":         r.SASHycoName,
		"E2E_SAS_LISTENER_KEY_NAME": r.ListenerKeyName,
		"E2E_SAS_LISTENER_KEY":      r.ListenerKey,
		"E2E_SAS_SENDER_KEY_NAME":   r.SenderKeyName,
		"E2E_SAS_SENDER_KEY":        r.SenderKey,
	}
}

// Provisioner creates and destroys per-invocation hybrid connections.
// Construct one with New, call Provision once, defer Teardown, and read
// Result for the test wiring.
type Provisioner struct {
	cfg    Config
	hycos  *armrelay.HybridConnectionsClient
	suffix string
	result *Result
}

// New constructs a Provisioner. It does not call Azure — that happens in
// Provision. The returned Provisioner is single-use.
//
// Prefer NewProvider for code paths that may need multiple provisions:
// Provisioner exists for back-compat with the original single-pair-per-
// process TestMain shape and is implemented today as a thin wrapper
// over the same per-call helpers Provider uses internally.
func New(cfg Config) (*Provisioner, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cred := cfg.Cred
	if cred == nil {
		c, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("default azure credential: %w", err)
		}
		cred = c
	}
	opts := cfg.ClientOptions
	if opts == nil {
		opts = DefaultClientOptions()
	}
	hycos, err := armrelay.NewHybridConnectionsClient(cfg.SubscriptionID, cred, opts)
	if err != nil {
		return nil, fmt.Errorf("new hybrid connections client: %w", err)
	}
	suffix, err := newSuffix()
	if err != nil {
		return nil, fmt.Errorf("generate suffix: %w", err)
	}
	return &Provisioner{cfg: cfg, hycos: hycos, suffix: suffix}, nil
}

// Provision creates the SAS hybrid connection (and the entra hybrid
// connection unless Config.SkipEntra is true, in which case
// Result.EntraHycoName is left empty) and stamps the namespace SAS
// rule key info from Config.RunRules onto the returned Result. If
// any step fails after a hyco has been created, Provision attempts
// a best-effort teardown before returning the error so the caller
// does not need to handle partial state.
//
// Authorization rules are NOT created here — they live at namespace
// scope and are provisioned once per `go test` invocation via
// AcquireRunRules. Config.RunRules must be non-nil.
func (p *Provisioner) Provision(ctx context.Context) (*Result, error) {
	if p.result != nil {
		return nil, errors.New("provisioner already used")
	}
	if p.cfg.RunRules == nil {
		return nil, errors.New("azrelay.Config.RunRules is required (call AcquireRunRules first)")
	}

	sasName := "e2e-sas-" + p.suffix
	var entraName string
	if !p.cfg.SkipEntra {
		entraName = "e2e-entra-" + p.suffix
		if err := p.createHyco(ctx, entraName); err != nil {
			// On error the hyco may or may not exist server-side (ARM
			// PUTs can fail post-create). Best-effort delete covers
			// the orphan case; the janitor will catch anything we
			// miss.
			p.bestEffortDelete(ctx, entraName)
			return nil, fmt.Errorf("create %s: %w", entraName, err)
		}
	}
	if err := p.createHyco(ctx, sasName); err != nil {
		if entraName != "" {
			p.bestEffortDelete(ctx, entraName)
		}
		p.bestEffortDelete(ctx, sasName)
		return nil, fmt.Errorf("create %s: %w", sasName, err)
	}

	rr := p.cfg.RunRules
	p.result = &Result{
		RelayName:       p.cfg.Namespace,
		EntraHycoName:   entraName,
		SASHycoName:     sasName,
		ListenerKeyName: rr.ListenerName,
		ListenerKey:     rr.ListenerKey,
		SenderKeyName:   rr.SenderName,
		SenderKey:       rr.SenderKey,
	}
	return p.result, nil
}

// Teardown deletes the hybrid connections created by Provision. Safe to
// call even if Provision failed partway through — entities that no longer
// exist are silently ignored. Callers typically defer this from TestMain.
//
// Teardown strips cancellation from ctx so it still runs when the
// caller's parent has been cancelled (e.g. on test timeout) but it
// preserves any deadline the caller set so the cleanup budget is
// what the caller declared. If ctx has no deadline, Teardown applies
// a defensive 60s ceiling so a stuck control plane cannot hang the
// run indefinitely.
//
// Note: authorization rules are not deleted here — they live at
// namespace scope and have a separate lifecycle owned by RunRules.
func (p *Provisioner) Teardown(ctx context.Context) error {
	if p.result == nil {
		return nil
	}
	ctx, cancel := detachAndBoundContext(ctx, 60*time.Second)
	defer cancel()

	var errs []error
	if p.result.EntraHycoName != "" {
		if _, err := p.hycos.Delete(ctx, p.cfg.ResourceGroup, p.cfg.Namespace, p.result.EntraHycoName, nil); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("delete %s: %w", p.result.EntraHycoName, err))
		}
	}
	if _, err := p.hycos.Delete(ctx, p.cfg.ResourceGroup, p.cfg.Namespace, p.result.SASHycoName, nil); err != nil && !isNotFound(err) {
		errs = append(errs, fmt.Errorf("delete %s: %w", p.result.SASHycoName, err))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// HycoNames returns the (entra, sas) names this provisioner holds.
// After a successful Provision the names come from the recorded
// Result, so a SkipEntra-mode Provisioner returns
// ("", "e2e-sas-<suffix>"). Before Provision the names are
// synthesised from the suffix, with the entra name elided to "" when
// Config.SkipEntra is true so callers see the same shape they will
// see post-Provision.
func (p *Provisioner) HycoNames() (entra, sas string) {
	if p.result != nil {
		return p.result.EntraHycoName, p.result.SASHycoName
	}
	sas = "e2e-sas-" + p.suffix
	if !p.cfg.SkipEntra {
		entra = "e2e-entra-" + p.suffix
	}
	return entra, sas
}

func (p *Provisioner) createHyco(ctx context.Context, name string) error {
	requiresAuth := true
	_, err := p.hycos.CreateOrUpdate(ctx, p.cfg.ResourceGroup, p.cfg.Namespace, name, armrelay.HybridConnection{
		Properties: &armrelay.HybridConnectionProperties{
			RequiresClientAuthorization: &requiresAuth,
		},
	}, nil)
	return err
}

func (p *Provisioner) bestEffortDelete(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	// Failure-path cleanup; other errors (404, 403, ...) are swallowed
	// by best-effort semantics.
	_, _ = p.hycos.Delete(ctx, p.cfg.ResourceGroup, p.cfg.Namespace, name, nil)
}

// newSuffix returns a fresh suffixLen-character lowercase hex string.
func newSuffix() (string, error) {
	raw := make([]byte, suffixLen/2)
	if _, err := crand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// authRuleRetry parameterises the bounded retry loop used to absorb the
// transient 40901 MessagingGatewayTooManyRequests conflict on Relay
// authorizationRule mutations. Kept as a struct so tests can drive the
// loop with tiny delays without sleeping for real.
type authRuleRetry struct {
	maxAttempts  int
	initialDelay time.Duration
	maxDelay     time.Duration
}

func defaultAuthRuleRetry() authRuleRetry {
	return authRuleRetry{
		maxAttempts:  authRuleMaxAttempts,
		initialDelay: authRuleInitialDelay,
		maxDelay:     authRuleMaxDelay,
	}
}

// RetryOnAuthRuleConflict invokes fn, retrying with the package's
// default backoff schedule while fn returns a transient Azure Relay
// 40901 conflict on an authorizationRule mutation. Any other error —
// including generic 429s — is returned to the caller immediately.
// The function honours ctx cancellation between retries.
//
// Exposed for use by callers outside this package that mutate the same
// Microsoft.Relay authorizationRules scope — currently `e2e-infra`'s
// namespace SAS-rule provisioning (Provisioner.EnsureRunRules) — so
// they share the same 40901 retry envelope.
func RetryOnAuthRuleConflict(ctx context.Context, fn func() error) error {
	return retryOnAuthRuleConflict(ctx, defaultAuthRuleRetry(), fn)
}

// retryOnAuthRuleConflict invokes fn, retrying with jittered exponential
// backoff while fn returns a transient Azure Relay 40901 conflict (see
// isAuthRuleConflict). Any other error — including generic 429s — is
// returned to the caller immediately so we never paper over a real fault.
// The function honours ctx cancellation between retries.
func retryOnAuthRuleConflict(ctx context.Context, cfg authRuleRetry, fn func() error) error {
	var lastErr error
	delay := cfg.initialDelay
	for attempt := 1; attempt <= cfg.maxAttempts; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if !isAuthRuleConflict(err) {
			return err
		}
		lastErr = err
		if attempt == cfg.maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jitter(delay)):
		}
		if delay < cfg.maxDelay {
			delay *= 2
			if delay > cfg.maxDelay {
				delay = cfg.maxDelay
			}
		}
	}
	return fmt.Errorf("after %d attempts: %w", cfg.maxAttempts, lastErr)
}

// isAuthRuleConflict reports whether err is the transient Azure Relay
// 40901 MessagingGatewayTooManyRequests conflict produced when sequential
// authorizationRule mutations race the per-scope serialization gate
// (where "scope" is either a hybrid connection or — for run-scoped
// rules — the namespace itself). Only this specific class is retried;
// generic 429s, other ARM errors, and other SubCodes under the same
// ErrorCode (e.g. namespace-level throttling that wouldn't benefit
// from short-window backoff against a single rule's commit window)
// are returned as-is.
//
// The SubCode marker is matched against ResponseError.Error() — azcore
// embeds the raw response body in that string, and the body is the
// authoritative carrier of the SubCode value in Service Bus / Relay
// control-plane responses.
func isAuthRuleConflict(err error) bool {
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) {
		return false
	}
	if respErr.StatusCode != http.StatusTooManyRequests ||
		respErr.ErrorCode != "MessagingGatewayTooManyRequests" {
		return false
	}
	return strings.Contains(respErr.Error(), "SubCode=40901")
}

// jitter returns a duration in the range [d/2, d]. Equal jitter is chosen
// over full jitter so backoff still grows monotonically in expectation —
// the race is a narrow per-hyco serialization window, not a thundering
// herd against a shared fleet, so we want to keep growing the gap between
// retries rather than risk re-firing near zero.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	// #nosec G404 -- jitter is timing noise, not a security boundary.
	return half + time.Duration(mrand.Int64N(int64(half)+1))
}

// isTransient classifies an error from ARM as retriable. Newly-created
// auth rules occasionally surface as missing or unauthorised for a short
// window before propagation completes; ARM also returns 5xx for its own
// internal hiccups. We treat 404, 401, and any 5xx as transient. Other
// statuses (e.g. 400, 403) are returned to the caller immediately so
// configuration errors are not masked behind a long retry loop.
func isTransient(err error) bool {
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) {
		return false
	}
	switch {
	case respErr.StatusCode == 404:
		return true
	case respErr.StatusCode == 401:
		return true
	case respErr.StatusCode >= 500 && respErr.StatusCode < 600:
		return true
	}
	return false
}

func (c Config) validate() error {
	switch {
	case c.SubscriptionID == "":
		return errors.New("azrelay.Config.SubscriptionID is required")
	case c.ResourceGroup == "":
		return errors.New("azrelay.Config.ResourceGroup is required")
	case c.Namespace == "":
		return errors.New("azrelay.Config.Namespace is required")
	}
	return nil
}
