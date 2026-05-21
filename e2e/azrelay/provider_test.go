package azrelay

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

func TestNewProvider_RejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"missing subscription", Config{ResourceGroup: "rg", Namespace: "ns", RunRules: stubRunRules()}, "SubscriptionID"},
		{"missing resource group", Config{SubscriptionID: "sub", Namespace: "ns", RunRules: stubRunRules()}, "ResourceGroup"},
		{"missing namespace", Config{SubscriptionID: "sub", ResourceGroup: "rg", RunRules: stubRunRules()}, "Namespace"},
		{"missing run rules", Config{SubscriptionID: "sub", ResourceGroup: "rg", Namespace: "ns"}, "RunRules"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewProvider(tc.cfg)
			if err == nil {
				t.Fatalf("NewProvider: expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewProvider: error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestNewProvider_DefaultsConcurrencyAndClientOptions(t *testing.T) {
	p, err := NewProvider(Config{
		SubscriptionID: "sub",
		ResourceGroup:  "rg",
		Namespace:      "ns",
		Cred:           stubCred{},
		RunRules:       stubRunRules(),
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if cap(p.sem) != DefaultProvisionerConcurrency {
		t.Errorf("sem cap = %d, want default %d", cap(p.sem), DefaultProvisionerConcurrency)
	}
}

func TestNewProvider_HonoursExplicitConcurrency(t *testing.T) {
	p, err := NewProvider(Config{
		SubscriptionID: "sub",
		ResourceGroup:  "rg",
		Namespace:      "ns",
		Cred:           stubCred{},
		Concurrency:    2,
		RunRules:       stubRunRules(),
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if cap(p.sem) != 2 {
		t.Errorf("sem cap = %d, want 2", cap(p.sem))
	}
}

func TestDefaultClientOptions_AppliesTunedRetryPolicy(t *testing.T) {
	opts := DefaultClientOptions()
	if opts == nil {
		t.Fatal("DefaultClientOptions returned nil")
	}
	if got := opts.Retry.MaxRetries; got != DefaultARMMaxRetries {
		t.Errorf("Retry.MaxRetries = %d, want %d", got, DefaultARMMaxRetries)
	}
	if got := opts.Retry.MaxRetryDelay; got != DefaultARMMaxRetryDelay {
		t.Errorf("Retry.MaxRetryDelay = %v, want %v", got, DefaultARMMaxRetryDelay)
	}
}

// TestProvider_AcquireRespectsConcurrencyLimit asserts that acquire
// returns immediately while the semaphore has slack and blocks once
// the limit is reached. The test does not invoke any ARM operations
// — it drives the semaphore directly through acquire/release so it
// exercises the bounding behaviour without needing a stub for ARM.
func TestProvider_AcquireRespectsConcurrencyLimit(t *testing.T) {
	p := &Provider{sem: make(chan struct{}, 2)}
	ctx := t.Context()

	if err := p.acquire(ctx); err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	if err := p.acquire(ctx); err != nil {
		t.Fatalf("acquire 2: %v", err)
	}

	// A third acquire must block until a slot is released. Drive that
	// by attempting acquire with a deadline shorter than any plausible
	// scheduler delay and expecting the deadline to fire.
	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	err := p.acquire(deadline)
	if err == nil {
		t.Fatal("acquire 3 with full semaphore should have blocked until ctx deadline")
	}
	if !strings.Contains(err.Error(), "concurrency slot") {
		t.Errorf("acquire 3 err = %v; want a wrap mentioning 'concurrency slot'", err)
	}

	// Release one slot and a fresh acquire must succeed.
	p.release()
	if err := p.acquire(ctx); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

// TestProvider_AcquireSerialisesBeyondCap drives 8 concurrent acquire
// calls against a cap-2 semaphore and asserts the peak in-flight count
// never exceeds the cap. Surfaces a bug where the semaphore is sized
// or used incorrectly more reliably than a single-goroutine test.
func TestProvider_AcquireSerialisesBeyondCap(t *testing.T) {
	const semSize = 2
	const goroutines = 8
	p := &Provider{sem: make(chan struct{}, semSize)}

	var inflight atomic.Int64
	var peak atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if err := p.acquire(t.Context()); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			cur := inflight.Add(1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			// Hold the slot briefly to give peers a chance to race.
			time.Sleep(2 * time.Millisecond)
			inflight.Add(-1)
			p.release()
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > int64(semSize) {
		t.Fatalf("peak in-flight = %d, want <= %d (semaphore breach)", got, semSize)
	}
}

// TestPairToken_TeardownInvokesDeleteOncePerHyco asserts that the
// first Teardown call invokes deleteFn exactly twice (entra + sas)
// and that subsequent calls do not re-enter deleteFn. This is the
// "delete-side" half of the production idempotency contract.
func TestPairToken_TeardownInvokesDeleteOncePerHyco(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	var capturedNames []string
	tok := &PairToken{
		suffix: "0123456789ab",
		deleteFn: func(ctx context.Context, name string) error {
			calls.Add(1)
			mu.Lock()
			capturedNames = append(capturedNames, name)
			mu.Unlock()
			return nil
		},
	}

	if err := tok.Teardown(t.Context()); err != nil {
		t.Fatalf("first teardown: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("first call invoked deleteFn %d times, want 2", got)
	}
	sort.Strings(capturedNames)
	want := []string{"e2e-entra-0123456789ab", "e2e-sas-0123456789ab"}
	if !reflect.DeepEqual(capturedNames, want) {
		t.Errorf("deleted names = %v, want %v", capturedNames, want)
	}

	if err := tok.Teardown(t.Context()); err != nil {
		t.Fatalf("second teardown: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("after second call, deleteFn invoked %d times, want still 2", got)
	}

	if err := tok.Teardown(t.Context()); err != nil {
		t.Fatalf("third teardown: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("after third call, deleteFn invoked %d times, want still 2", got)
	}
}

// TestPairToken_TeardownReplaysError asserts that an error from the
// first Teardown is returned identically on every subsequent call —
// callers polling Teardown for cleanup status get the same answer
// each time. This is the "error-side" half of the production
// idempotency contract.
func TestPairToken_TeardownReplaysError(t *testing.T) {
	sentinel := errors.New("boom")
	var calls atomic.Int32
	tok := &PairToken{
		suffix: "deadbeef0000",
		deleteFn: func(ctx context.Context, name string) error {
			calls.Add(1)
			return sentinel
		},
	}

	err1 := tok.Teardown(t.Context())
	if err1 == nil || !errors.Is(err1, sentinel) {
		t.Fatalf("first teardown err = %v; want errors.Is(sentinel)", err1)
	}
	err2 := tok.Teardown(t.Context())
	if err2 != err1 {
		t.Errorf("second teardown err = %v; want same value as first (%v)", err2, err1)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("deleteFn invoked %d times across 2 teardowns, want 2", got)
	}
}

func TestPairToken_HycoNamesMirrorSuffix(t *testing.T) {
	tok := &PairToken{suffix: "abcdef012345"}
	entra, sas := tok.HycoNames()
	if entra != "e2e-entra-abcdef012345" {
		t.Errorf("entra = %q", entra)
	}
	if sas != "e2e-sas-abcdef012345" {
		t.Errorf("sas = %q", sas)
	}
	for _, n := range []string{entra, sas} {
		if !HycoNamePattern.MatchString(n) {
			t.Errorf("name %q does not match janitor pattern", n)
		}
	}
}

func TestPairToken_ResultPassesThrough(t *testing.T) {
	r := &Result{EntraHycoName: "x", SASHycoName: "y"}
	tok := &PairToken{result: r}
	if tok.Result() != r {
		t.Fatal("Result() did not return the stored *Result")
	}
}

// TestPairToken_HycoNamesSkipEntraSurfacesEmpty asserts that when a
// PairToken has a populated Result whose EntraHycoName is empty
// (SkipEntra mode), HycoNames returns the empty entra name from the
// Result rather than falling back to the suffix-derived synthetic
// name. This locks in "result wins over suffix" at the PairToken
// level.
func TestPairToken_HycoNamesSkipEntraSurfacesEmpty(t *testing.T) {
	const suffix = "00112233aabb"
	tok := &PairToken{
		suffix: suffix,
		result: &Result{
			EntraHycoName: "",
			SASHycoName:   "e2e-sas-" + suffix,
		},
	}
	entra, sas := tok.HycoNames()
	if entra != "" {
		t.Errorf("entra = %q, want empty (Result wins over suffix)", entra)
	}
	if sas != "e2e-sas-"+suffix {
		t.Errorf("sas = %q, want %q", sas, "e2e-sas-"+suffix)
	}
}

// TestPairToken_TeardownSkipsEntraWhenResultEntraEmpty asserts that
// Teardown invokes deleteFn exactly once (SAS only) when the Result
// records SkipEntra (EntraHycoName empty), and that the count holds
// across repeat calls.
func TestPairToken_TeardownSkipsEntraWhenResultEntraEmpty(t *testing.T) {
	const suffix = "feedface1234"
	var calls atomic.Int32
	var mu sync.Mutex
	var capturedNames []string
	tok := &PairToken{
		suffix: suffix,
		result: &Result{
			EntraHycoName: "",
			SASHycoName:   "e2e-sas-" + suffix,
		},
		deleteFn: func(ctx context.Context, name string) error {
			calls.Add(1)
			mu.Lock()
			capturedNames = append(capturedNames, name)
			mu.Unlock()
			return nil
		},
	}

	if err := tok.Teardown(t.Context()); err != nil {
		t.Fatalf("first teardown: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("deleteFn invoked %d times, want 1 (SkipEntra: SAS only)", got)
	}
	if len(capturedNames) != 1 || capturedNames[0] != "e2e-sas-"+suffix {
		t.Errorf("captured names = %v, want [%q]", capturedNames, "e2e-sas-"+suffix)
	}

	if err := tok.Teardown(t.Context()); err != nil {
		t.Fatalf("second teardown: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("after second teardown, deleteFn invoked %d times, want still 1", got)
	}
}

// TestPairToken_TeardownErrorOnSASOnly asserts that a deleteFn error
// on the SAS-only Teardown path is wrapped, replayed identically on
// subsequent calls, and that the underlying sentinel is recoverable
// via errors.Is — matching the production idempotency contract for
// the both-hycos path.
func TestPairToken_TeardownErrorOnSASOnly(t *testing.T) {
	const suffix = "deadbabe5678"
	sentinel := errors.New("sas-only boom")
	var calls atomic.Int32
	tok := &PairToken{
		suffix: suffix,
		result: &Result{
			EntraHycoName: "",
			SASHycoName:   "e2e-sas-" + suffix,
		},
		deleteFn: func(ctx context.Context, name string) error {
			calls.Add(1)
			return sentinel
		},
	}

	err1 := tok.Teardown(t.Context())
	if err1 == nil || !errors.Is(err1, sentinel) {
		t.Fatalf("first teardown err = %v; want errors.Is(sentinel)", err1)
	}
	err2 := tok.Teardown(t.Context())
	if err2 != err1 {
		t.Errorf("second teardown err = %v; want same value as first (%v)", err2, err1)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("deleteFn invoked %d times across 2 teardowns, want 1 (SkipEntra)", got)
	}
}

// TestPairToken_TeardownHonoursCallerDeadline verifies that a
// deadline set on the caller's context is preserved across the
// cancellation strip in Teardown. Specifically: a deadline shorter
// than the fallback 60s ceiling should fire and abort the deletes.
// Without this contract, callers wiring an explicit budget into
// requireDedicatedHyco's t.Cleanup would silently fall back to the
// internal ceiling.
func TestPairToken_TeardownHonoursCallerDeadline(t *testing.T) {
	var calls atomic.Int32
	tok := &PairToken{
		suffix: "feedface0001",
		deleteFn: func(ctx context.Context, name string) error {
			calls.Add(1)
			// Block until the context fires so we can observe
			// whether the caller's deadline propagated.
			<-ctx.Done()
			return ctx.Err()
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := tok.Teardown(ctx); err == nil {
		t.Fatal("expected Teardown to surface context deadline error")
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("Teardown ran for %v; caller's 30ms deadline was not honoured (internal 60s ceiling leaked through)", elapsed)
	}
	if got := calls.Load(); got < 1 {
		t.Errorf("deleteFn invoked %d times, want >= 1 (first delete should have started)", got)
	}
}

// TestPairToken_TeardownStripsCallerCancellation asserts that an
// already-cancelled caller context does NOT short-circuit Teardown —
// cleanup must still run so test-timeout aborts don't orphan hycos.
// We use a context that's both cancelled AND has a deadline well in
// the future; Teardown should ignore the cancellation, honour the
// future deadline, and complete the deletes.
func TestPairToken_TeardownStripsCallerCancellation(t *testing.T) {
	var calls atomic.Int32
	tok := &PairToken{
		suffix: "deadbabe0002",
		deleteFn: func(ctx context.Context, name string) error {
			calls.Add(1)
			return nil
		},
	}
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	cancel()

	if err := tok.Teardown(parent); err != nil {
		t.Fatalf("Teardown returned err on already-cancelled parent: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("deleteFn invoked %d times, want 2 (both deletes should have run despite parent cancellation)", got)
	}
}

// TestPairToken_TeardownGatesOnProviderSemaphore asserts that Teardown
// acquires the same concurrency slot used by Provision, so a swarm of
// test cleanups cannot stampede the relay control plane beyond the
// per-Provider cap. The test pre-saturates a cap-1 semaphore, runs
// Teardown in a goroutine, observes that deleteFn does NOT run until
// the slot is freed, and then asserts deleteFn runs once the slot is
// released.
func TestPairToken_TeardownGatesOnProviderSemaphore(t *testing.T) {
	p := &Provider{sem: make(chan struct{}, 1)}
	// Hold the only slot from the test goroutine.
	if err := p.acquire(t.Context()); err != nil {
		t.Fatalf("seed acquire: %v", err)
	}

	var calls atomic.Int32
	tok := &PairToken{
		provider: p,
		suffix:   "00112233aabb",
		deleteFn: func(ctx context.Context, name string) error {
			calls.Add(1)
			return nil
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tok.Teardown(context.Background())
	}()

	// Give Teardown a chance to block on the saturated sem; deleteFn
	// must NOT have run yet.
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("Teardown ran deleteFn while sem was saturated: calls = %d", got)
	}

	// Release the slot; Teardown should now complete and run deleteFn
	// twice (entra + sas).
	p.release()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Teardown did not complete after sem release")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("deleteFn invoked %d times after sem release, want 2", got)
	}
}

// stubCred satisfies azcore.TokenCredential without producing a token.
// NewProvider only needs Cred to construct the ARM client; it does not
// invoke GetToken in the constructor path. The empty implementation
// would panic if a test ever drove a request through it.
type stubCred struct{}

func (stubCred) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, nil
}
