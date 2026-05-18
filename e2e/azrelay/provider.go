package azrelay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/relay/armrelay"
)

// DefaultProvisionerConcurrency caps the number of in-flight Provision
// calls a Provider runs concurrently. Each provision performs ~6 ARM
// round-trips against the same namespace; running many in parallel
// invites the namespace-level 429 throttling envelope. Empirically a
// concurrency of 4 stays well clear of the threshold while still
// overlapping enough provisioning to hide the ARM tail in test-body
// time when the e2e suite runs with -parallel=GOMAXPROCS.
//
// Override at the call site by setting Config.Concurrency.
const DefaultProvisionerConcurrency = 4

// DefaultARMMaxRetries widens azcore's default 3 retries to 6 for
// transient 429s and 5xx on the per-test provisioning path. The SDK's
// retry policy already honours `Retry-After` headers when present, so
// 6 attempts comfortably absorbs a brief namespace-level throttling
// burst (~Retry-After=30s × 6 = ~3 min total budget) without us
// stacking an outer retry loop that would fight the SDK's own backoff.
const DefaultARMMaxRetries = 6

// DefaultARMMaxRetryDelay caps any single SDK-managed retry delay.
// azcore's default is already 60 s; we set it explicitly so it stays
// visible if a future SDK release changes the default.
const DefaultARMMaxRetryDelay = 60 * time.Second

// Provider creates fresh hybrid-connection pairs on demand. Safe for
// concurrent use by multiple goroutines: ARM operations are gated by
// an internal semaphore so a swarm of t.Parallel tests cannot stampede
// the relay control plane.
//
// Construct one with NewProvider, then call Provision per test. Each
// successful call returns a single-use PairToken whose Teardown must
// be invoked (typically via t.Cleanup) to release the underlying
// Azure resources.
type Provider struct {
	cfg   Config
	cred  azcore.TokenCredential
	hycos *armrelay.HybridConnectionsClient
	sem   chan struct{}
}

// NewProvider builds a Provider bound to cfg. It performs no Azure
// I/O — the ARM client is constructed eagerly but no requests are
// issued until the first Provision call.
//
// If cfg.Cred is nil, NewProvider constructs a DefaultAzureCredential.
// If cfg.ClientOptions is nil, NewProvider applies a per-test-tuned
// retry policy (DefaultARMMaxRetries / DefaultARMMaxRetryDelay) so
// transient 429s are absorbed by the SDK pipeline.
// If cfg.Concurrency is zero, DefaultProvisionerConcurrency is used.
func NewProvider(cfg Config) (*Provider, error) {
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
	conc := cfg.Concurrency
	if conc <= 0 {
		conc = DefaultProvisionerConcurrency
	}
	return &Provider{
		cfg:   cfg,
		cred:  cred,
		hycos: hycos,
		sem:   make(chan struct{}, conc),
	}, nil
}

// DefaultClientOptions returns the arm.ClientOptions used by NewProvider
// when Config.ClientOptions is nil. Exported so callers building their
// own ClientOptions (e.g. with a custom user-agent or test-only HTTP
// transport) can layer their tweaks on top of the per-test-tuned
// retry policy.
func DefaultClientOptions() *arm.ClientOptions {
	return &arm.ClientOptions{
		ClientOptions: policy.ClientOptions{
			Retry: policy.RetryOptions{
				MaxRetries:    DefaultARMMaxRetries,
				MaxRetryDelay: DefaultARMMaxRetryDelay,
			},
		},
	}
}

// Provision creates a fresh (entra, sas) hybrid-connection pair using
// the same sequence as Provisioner.Provision: create entra hyco,
// create sas hyco, attach Listen + Send authorization rules to the
// sas hyco, read both SAS keys, and let the data plane settle. The
// returned PairToken's Teardown method releases both hycos.
//
// Provision blocks if the concurrency semaphore is full. The block
// honours ctx.Done so callers (typically t.Cleanup-registered)
// observe cancellation.
//
// If Provision returns an error, the caller does not need to call
// Teardown — partial state is cleaned up best-effort internally.
func (p *Provider) Provision(ctx context.Context) (*PairToken, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.release()

	suffix, err := newSuffix()
	if err != nil {
		return nil, fmt.Errorf("generate suffix: %w", err)
	}
	inner := &Provisioner{
		cfg:    p.cfg,
		hycos:  p.hycos,
		suffix: suffix,
	}
	result, err := inner.Provision(ctx)
	if err != nil {
		return nil, err
	}
	return &PairToken{
		provider: p,
		result:   result,
		suffix:   suffix,
		deleteFn: func(ctx context.Context, name string) error {
			_, err := p.hycos.Delete(ctx, p.cfg.ResourceGroup, p.cfg.Namespace, name, nil)
			return err
		},
	}, nil
}

func (p *Provider) acquire(ctx context.Context) error {
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for provisioner concurrency slot: %w", ctx.Err())
	}
}

func (p *Provider) release() {
	<-p.sem
}

// PairToken represents one successfully provisioned hyco pair. It is
// single-use: a second Teardown call returns the same error as the
// first without re-issuing any ARM Delete requests.
type PairToken struct {
	provider *Provider
	result   *Result
	suffix   string

	// deleteFn issues a single ARM Delete against the named hyco.
	// Provider.Provision populates this with a closure over the
	// shared *armrelay.HybridConnectionsClient; tests override it to
	// drive Teardown without ARM I/O.
	deleteFn func(ctx context.Context, name string) error

	teardownOnce sync.Once
	teardownErr  error
}

// Result returns the connection metadata for this pair. Never nil for
// a token returned by a successful Provision call.
func (t *PairToken) Result() *Result {
	return t.result
}

// HycoNames returns the (entra, sas) names this token holds. Useful
// for log messages.
func (t *PairToken) HycoNames() (entra, sas string) {
	return "e2e-entra-" + t.suffix, "e2e-sas-" + t.suffix
}

// Teardown deletes both hybrid connections. Safe to call multiple
// times: only the first call performs the deletes; subsequent calls
// return the same error.
//
// Teardown strips cancellation from ctx (via context.WithoutCancel)
// so cleanup completes even when the test's ctx has been cancelled
// (e.g. on test timeout), but it preserves any deadline the caller
// set so the cleanup budget is what the caller declared. If ctx has
// no deadline, Teardown applies a defensive 60s ceiling so a stuck
// control plane cannot hang the run indefinitely; callers wanting a
// longer budget must pass a context with that deadline themselves.
//
// When the token was created by Provider.Provision (i.e. provider
// and deleteFn are non-nil) Teardown also acquires the provider's
// concurrency slot for the duration of the ARM Delete calls so a
// wave of test cleanups cannot stampede the relay control plane and
// exhaust the namespace 429 envelope. If the slot acquire fails
// (e.g. the deadline expires while the sem is saturated) Teardown
// proceeds without the slot rather than orphan the hyco — the
// janitor will reap anything we leak.
//
// Individual delete failures are joined and returned. The janitor
// will also reap anything we can't clean up here.
func (t *PairToken) Teardown(ctx context.Context) error {
	t.teardownOnce.Do(func() {
		ctx, cancel := detachAndBoundContext(ctx, 60*time.Second)
		defer cancel()
		if t.provider != nil {
			if err := t.provider.acquire(ctx); err == nil {
				defer t.provider.release()
			}
		}
		var errs []error
		entra, sas := t.HycoNames()
		if err := t.deleteFn(ctx, entra); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", entra, err))
		}
		if err := t.deleteFn(ctx, sas); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", sas, err))
		}
		if len(errs) > 0 {
			t.teardownErr = errors.Join(errs...)
		}
	})
	return t.teardownErr
}

// detachAndBoundContext returns a context that ignores ctx's
// cancellation but preserves ctx's deadline. If ctx has no deadline
// the returned context applies fallback as the deadline so callers
// passing context.Background() still get a hang ceiling. Used by
// Teardown / Provisioner.Teardown to decouple cleanup from the
// caller's cancellation (so test timeouts don't abort cleanup) while
// still honouring an explicit cleanup budget the caller wired in.
func detachAndBoundContext(ctx context.Context, fallback time.Duration) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	ctx = context.WithoutCancel(ctx)
	if ok {
		return context.WithDeadline(ctx, deadline)
	}
	return context.WithTimeout(ctx, fallback)
}
