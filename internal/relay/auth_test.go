package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

func TestSanitizeErr(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"single token", "dial wss://host/$hc?sb-hc-action=connect&sb-hc-token=SECRET"},
		{"token at end", "error sb-hc-token=SECRET"},
		{"token with trailing space", "error sb-hc-token=SECRET rest of message"},
		{"token with trailing quote", `error sb-hc-token=SECRET" more`},
		{"multiple tokens", "first sb-hc-token=SECRET1 second sb-hc-token=SECRET2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := sanitizeErr(fmt.Errorf("%s", tt.input))
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("token not redacted: %v", err)
			}
			if !strings.Contains(err.Error(), "REDACTED") {
				t.Errorf("expected REDACTED in error: %v", err)
			}
		})
	}

	t.Run("no token", func(t *testing.T) {
		err := sanitizeErr(fmt.Errorf("connection refused"))
		if err.Error() != "connection refused" {
			t.Errorf("expected unchanged error, got %q", err.Error())
		}
	})

	t.Run("preserves error chain", func(t *testing.T) {
		orig := fmt.Errorf("outer: %w", context.DeadlineExceeded)
		err := sanitizeErr(orig)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Error("sanitizeErr should preserve error chain for errors.Is")
		}
	})
}

// programmableCredential is a configurable azcore.TokenCredential used to
// exercise EntraTokenProvider's caching behaviour. Tests mutate its fields
// between GetToken calls to control the underlying response, with the
// internal mutex guarding concurrent reads from the credential and concurrent
// writes from the test driver. The fields can be tuned to inject errors,
// simulate fetch latency, or return tokens whose ExpiresOn falls inside the
// EntraTokenProvider refresh skew.
type programmableCredential struct {
	mu        sync.Mutex
	calls     atomic.Int64
	token     string
	expiry    time.Time
	refreshOn time.Time
	err       error
	delay     time.Duration
}

func (c *programmableCredential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.calls.Add(1)
	c.mu.Lock()
	delay := c.delay
	err := c.err
	token := c.token
	expiry := c.expiry
	refreshOn := c.refreshOn
	c.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return azcore.AccessToken{}, ctx.Err()
		}
	}
	if err != nil {
		return azcore.AccessToken{}, err
	}
	if expiry.IsZero() {
		expiry = time.Now().Add(time.Hour)
	}
	if len(opts.Scopes) == 0 {
		return azcore.AccessToken{}, errors.New("programmableCredential: no scope set")
	}
	return azcore.AccessToken{Token: token, ExpiresOn: expiry, RefreshOn: refreshOn}, nil
}

// TestEntraTokenProvider_ConcurrentDedup verifies that N concurrent callers
// of EntraTokenProvider.GetToken result in exactly one underlying credential
// fetch. This is the property that fixes the AzureCLICredential serialisation
// cliff documented in issue #68: without it, eight concurrent sender dials
// would each pay the ~1.1s `az` shell-out cost in series.
func TestEntraTokenProvider_ConcurrentDedup(t *testing.T) {
	const goroutines = 64
	cred := &programmableCredential{
		token: "tok",
		// Force every caller to queue at the mutex by holding the first
		// fetch for long enough that scheduling cannot let any goroutine
		// race ahead. 50ms is comfortably above goroutine startup jitter
		// while keeping the test under a second.
		delay: 50 * time.Millisecond,
	}
	tp := NewEntraTokenProviderWithCredential(cred)

	start := make(chan struct{})
	var wg sync.WaitGroup
	tokens := make([]string, goroutines)
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tok, err := tp.GetToken(context.Background(), "ignored")
			tokens[i] = tok
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	if got := cred.calls.Load(); got != 1 {
		t.Errorf("underlying credential called %d times, want 1", got)
	}
	for i := range tokens {
		if errs[i] != nil {
			t.Errorf("goroutine %d: unexpected error %v", i, errs[i])
		}
		if tokens[i] != "tok" {
			t.Errorf("goroutine %d: token = %q, want %q", i, tokens[i], "tok")
		}
	}
}

// TestEntraTokenProvider_RefreshAfterSkew verifies that a cached token whose
// ExpiresOn sits inside the refresh skew is not reused: the next GetToken
// triggers a fresh underlying fetch and returns the new token. Without this
// behaviour a long-lived sender would silently keep handing out a stale token
// past its expiry, and the relay control plane would start rejecting dials.
func TestEntraTokenProvider_RefreshAfterSkew(t *testing.T) {
	cred := &programmableCredential{
		token: "tok-A",
		// Expire well inside the 5-minute refresh skew so the cache entry
		// is considered stale immediately. Using a positive but short
		// duration (rather than a past time) ensures the first call
		// still receives a non-empty token to cache; the *next* call
		// will see the stale entry and refresh.
		expiry: time.Now().Add(time.Minute),
	}
	tp := NewEntraTokenProviderWithCredential(cred)

	first, err := tp.GetToken(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("first GetToken: %v", err)
	}
	if first != "tok-A" {
		t.Fatalf("first token = %q, want %q", first, "tok-A")
	}

	cred.mu.Lock()
	cred.token = "tok-B"
	cred.expiry = time.Now().Add(time.Hour) // fresh, well past skew
	cred.mu.Unlock()

	second, err := tp.GetToken(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("second GetToken: %v", err)
	}
	if second != "tok-B" {
		t.Errorf("second token = %q, want %q (cache should have refreshed)", second, "tok-B")
	}
	if got := cred.calls.Load(); got != 2 {
		t.Errorf("underlying credential called %d times, want 2", got)
	}
}

// TestEntraTokenProvider_ErrorNotCached verifies that an error from the
// underlying credential is returned to the caller and NOT cached. A
// subsequent successful fetch must call the credential again and return the
// new token. Caching errors would lock the provider out of a recoverable
// auth blip (transient ADAL outage, race with az login) for the lifetime of
// the process.
func TestEntraTokenProvider_ErrorNotCached(t *testing.T) {
	sentinel := errors.New("upstream auth boom")
	cred := &programmableCredential{
		err: sentinel,
	}
	tp := NewEntraTokenProviderWithCredential(cred)

	_, err := tp.GetToken(context.Background(), "ignored")
	if !errors.Is(err, sentinel) {
		t.Fatalf("first GetToken error = %v, want wraps %v", err, sentinel)
	}

	cred.mu.Lock()
	cred.err = nil
	cred.token = "tok-recovered"
	cred.mu.Unlock()

	tok, err := tp.GetToken(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("recovery GetToken: %v", err)
	}
	if tok != "tok-recovered" {
		t.Errorf("recovery token = %q, want %q", tok, "tok-recovered")
	}
	if got := cred.calls.Load(); got != 2 {
		t.Errorf("underlying credential called %d times, want 2 (no caching of error)", got)
	}
}

// TestEntraTokenProvider_ContextCancellation verifies that a context cancelled
// during a refresh propagates through to the underlying credential and the
// resulting error is surfaced to the caller without poisoning the cache —
// the next call with a healthy context must succeed.
func TestEntraTokenProvider_ContextCancellation(t *testing.T) {
	cred := &programmableCredential{
		token: "tok",
		delay: 5 * time.Second,
	}
	tp := NewEntraTokenProviderWithCredential(cred)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := tp.GetToken(ctx, "ignored")
		done <- err
	}()

	// Give the goroutine a beat to enter GetToken and start blocking
	// on the credential's delay before we cancel; 50ms is well above
	// goroutine startup jitter and well below the 5s delay.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v, want wraps context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetToken did not return after ctx cancellation within 2s")
	}

	// Now reconfigure the credential for a fast successful fetch and
	// verify the cache wasn't poisoned by the cancelled call.
	cred.mu.Lock()
	cred.delay = 0
	cred.mu.Unlock()

	tok, err := tp.GetToken(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("post-cancel GetToken: %v", err)
	}
	if tok != "tok" {
		t.Errorf("post-cancel token = %q, want %q", tok, "tok")
	}
}

// TestEntraTokenProvider_WaiterContextCancellation verifies that a caller
// arriving while a refresh is already in flight respects its own context and
// returns promptly when the context is cancelled — without waiting for the
// in-flight refresh to finish. Without this property a hung underlying
// credential (for example a stuck `az` invocation) could block deadline-bound
// dials past their deadlines simply because they queued behind it.
func TestEntraTokenProvider_WaiterContextCancellation(t *testing.T) {
	cred := &programmableCredential{
		token: "tok",
		// Long enough that the waiter's short context must time out
		// before the refresher finishes.
		delay: 2 * time.Second,
	}
	tp := NewEntraTokenProviderWithCredential(cred)

	// Kick off a long-running refresh in the background. Use a context
	// without a deadline so the refresher is purely gated on the delay.
	refresherDone := make(chan struct{})
	go func() {
		_, _ = tp.GetToken(context.Background(), "ignored")
		close(refresherDone)
	}()

	// Wait deterministically for the background refresher to enter the
	// underlying credential's GetToken. EntraTokenProvider publishes
	// p.refreshing before calling cred.GetToken, and cred.GetToken
	// increments cred.calls before honoring the delay — so observing
	// calls == 1 strictly happens-after p.refreshing is published, which
	// guarantees the next caller enters the waiter branch.
	deadline := time.Now().Add(2 * time.Second)
	for cred.calls.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("background refresher never entered credential GetToken (calls=%d)", cred.calls.Load())
		}
		time.Sleep(time.Millisecond)
	}

	// A second caller with a short deadline must observe ctx cancellation
	// even though the refresher hasn't finished yet.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := tp.GetToken(ctx, "ignored")
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter error = %v, want wraps context.DeadlineExceeded", err)
	}
	// Allow generous slack for slow CI hosts, but ensure the waiter did
	// not block until the refresher's 2s delay completed.
	if elapsed > time.Second {
		t.Errorf("waiter took %s to return; expected ~100ms (refresher still running)", elapsed)
	}

	// The refresher should still be making forward progress; let it finish
	// so the test goroutine exits cleanly.
	<-refresherDone
	if got := cred.calls.Load(); got != 1 {
		t.Errorf("underlying credential called %d times, want 1 (waiter must not trigger a second fetch)", got)
	}
}

// TestEntraTokenProvider_RespectsRefreshOn verifies that the cache treats a
// non-zero RefreshOn as the refresh threshold (matching azcore's canonical
// BearerTokenPolicy.shouldRefresh): when RefreshOn is in the past, the next
// call must trigger a refresh even though ExpiresOn is still distant; when
// RefreshOn is in the future, the cache must remain a hit.
func TestEntraTokenProvider_RespectsRefreshOn(t *testing.T) {
	t.Run("past RefreshOn forces refresh despite distant ExpiresOn", func(t *testing.T) {
		cred := &programmableCredential{
			token:     "tok-1",
			expiry:    time.Now().Add(time.Hour),
			refreshOn: time.Now().Add(-time.Minute),
		}
		tp := NewEntraTokenProviderWithCredential(cred)

		tok, err := tp.GetToken(context.Background(), "ignored")
		if err != nil {
			t.Fatalf("first GetToken returned %v", err)
		}
		if tok != "tok-1" {
			t.Fatalf("first GetToken = %q, want %q", tok, "tok-1")
		}

		cred.mu.Lock()
		cred.token = "tok-2"
		cred.refreshOn = time.Time{}
		cred.mu.Unlock()

		tok, err = tp.GetToken(context.Background(), "ignored")
		if err != nil {
			t.Fatalf("second GetToken returned %v", err)
		}
		if tok != "tok-2" {
			t.Errorf("second GetToken = %q, want %q (past RefreshOn must invalidate cache)", tok, "tok-2")
		}
		if got := cred.calls.Load(); got != 2 {
			t.Errorf("underlying credential called %d times, want 2", got)
		}
	})

	t.Run("future RefreshOn keeps cache hot", func(t *testing.T) {
		cred := &programmableCredential{
			token:     "tok-1",
			expiry:    time.Now().Add(time.Hour),
			refreshOn: time.Now().Add(30 * time.Minute),
		}
		tp := NewEntraTokenProviderWithCredential(cred)

		for i := 0; i < 5; i++ {
			tok, err := tp.GetToken(context.Background(), "ignored")
			if err != nil {
				t.Fatalf("call %d: GetToken returned %v", i, err)
			}
			if tok != "tok-1" {
				t.Errorf("call %d: GetToken = %q, want %q", i, tok, "tok-1")
			}
		}
		if got := cred.calls.Load(); got != 1 {
			t.Errorf("underlying credential called %d times, want 1 (future RefreshOn must keep cache hot)", got)
		}
	})
}

// stubTokenProvider is a TokenProvider whose GetToken behaviour is
// configured per-test. delay simulates underlying credential latency;
// err, when set, replaces the token return. Used to drive
// metricsTokenProvider through both result paths without touching the
// real EntraTokenProvider or SASTokenProvider machinery.
type stubTokenProvider struct {
	delay time.Duration
	token string
	err   error
}

func (s *stubTokenProvider) GetToken(ctx context.Context, _ string) (string, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if s.err != nil {
		return "", s.err
	}
	return s.token, nil
}

// recordingObserver captures every ObserveTokenFetch call. Tests inspect
// observations directly rather than going through the full metrics
// package — relay's contract with TokenFetchObserver is what's under
// test here, not the metrics implementation.
type recordingObserver struct {
	mu           sync.Mutex
	observations []tokenFetchObservation
}

type tokenFetchObservation struct {
	provider    string
	result      string
	durationSec float64
}

func (r *recordingObserver) ObserveTokenFetch(provider, result string, durationSec float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observations = append(r.observations, tokenFetchObservation{
		provider:    provider,
		result:      result,
		durationSec: durationSec,
	})
}

func (r *recordingObserver) snapshot() []tokenFetchObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]tokenFetchObservation, len(r.observations))
	copy(out, r.observations)
	return out
}

// TestMetricsTokenProvider_RecordsLatency_OK verifies that a successful
// GetToken call yields one observation with result="ok" and a duration
// that brackets the underlying provider's actual latency. The bracket is
// loose (40-300ms for a 50ms inner delay) to absorb scheduler jitter on
// slow CI runners without losing the property under test: the wrapper
// records the wall-clock time spent inside the inner GetToken, not zero
// and not the test deadline.
func TestMetricsTokenProvider_RecordsLatency_OK(t *testing.T) {
	const innerDelay = 50 * time.Millisecond
	inner := &stubTokenProvider{token: "tok-ok", delay: innerDelay}
	obs := &recordingObserver{}
	tp := WithMetrics(inner, obs, "stub")

	tok, err := tp.GetToken(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if tok != "tok-ok" {
		t.Errorf("token = %q, want %q", tok, "tok-ok")
	}

	got := obs.snapshot()
	if len(got) != 1 {
		t.Fatalf("observations = %d, want 1", len(got))
	}
	if got[0].provider != "stub" {
		t.Errorf("provider = %q, want %q", got[0].provider, "stub")
	}
	if got[0].result != "ok" {
		t.Errorf("result = %q, want %q", got[0].result, "ok")
	}
	if got[0].durationSec < 0.04 || got[0].durationSec > 0.30 {
		t.Errorf("durationSec = %v, want in [0.04, 0.30] for %v inner delay",
			got[0].durationSec, innerDelay)
	}
}

// TestMetricsTokenProvider_RecordsLatency_Error verifies that a failing
// GetToken call yields one observation with result="error", the error is
// surfaced to the caller unchanged (errors.Is matches the sentinel), and
// the duration still reflects the time spent inside the inner provider.
func TestMetricsTokenProvider_RecordsLatency_Error(t *testing.T) {
	sentinel := errors.New("boom")
	inner := &stubTokenProvider{err: sentinel, delay: 20 * time.Millisecond}
	obs := &recordingObserver{}
	tp := WithMetrics(inner, obs, "stub")

	_, err := tp.GetToken(context.Background(), "ignored")
	if !errors.Is(err, sentinel) {
		t.Fatalf("GetToken error = %v, want wraps %v", err, sentinel)
	}

	got := obs.snapshot()
	if len(got) != 1 {
		t.Fatalf("observations = %d, want 1", len(got))
	}
	if got[0].result != "error" {
		t.Errorf("result = %q, want %q", got[0].result, "error")
	}
	if got[0].durationSec <= 0 {
		t.Errorf("durationSec = %v, want > 0", got[0].durationSec)
	}
}

// TestMetricsTokenProvider_LabelsCorrect verifies the provider label
// passed to WithMetrics is what shows up on the observation. Covers both
// the documented ProviderSAS / ProviderEntra constants and an arbitrary
// custom string, ensuring there is no hidden allow-list of labels.
func TestMetricsTokenProvider_LabelsCorrect(t *testing.T) {
	for _, want := range []string{ProviderSAS, ProviderEntra, "custom"} {
		t.Run(want, func(t *testing.T) {
			inner := &stubTokenProvider{token: "tok"}
			obs := &recordingObserver{}
			tp := WithMetrics(inner, obs, want)
			if _, err := tp.GetToken(context.Background(), "ignored"); err != nil {
				t.Fatalf("GetToken: %v", err)
			}
			got := obs.snapshot()
			if len(got) != 1 || got[0].provider != want {
				t.Errorf("provider label = %q, want %q (observations=%#v)",
					func() string {
						if len(got) == 0 {
							return ""
						}
						return got[0].provider
					}(), want, got)
			}
		})
	}
}

// TestWithMetrics_NilObserverPassThrough verifies that an untyped-nil
// observer makes WithMetrics a no-op: the caller gets back the original
// TokenProvider unchanged, so calls bypass the wrapper entirely. This
// keeps WithMetrics safe to call from any construction pipeline that
// may not yet have observability wired in, without forcing callers to
// branch on a nil observer themselves.
func TestWithMetrics_NilObserverPassThrough(t *testing.T) {
	inner := &stubTokenProvider{token: "tok"}
	tp := WithMetrics(inner, nil, "stub")
	if tp != TokenProvider(inner) {
		t.Errorf("WithMetrics(_, nil, _) should return the inner provider unchanged, got %T", tp)
	}
	tok, err := tp.GetToken(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if tok != "tok" {
		t.Errorf("token = %q, want %q", tok, "tok")
	}
}

// TestMetricsTokenProvider_PreservesError verifies that the wrapper
// returns the inner error reference unmodified (not a wrapped or copied
// error) so errors.Is/As callers see exactly what the underlying
// provider produced.
func TestMetricsTokenProvider_PreservesError(t *testing.T) {
	sentinel := errors.New("inner")
	inner := &stubTokenProvider{err: sentinel}
	obs := &recordingObserver{}
	tp := WithMetrics(inner, obs, "stub")

	_, err := tp.GetToken(context.Background(), "ignored")
	if err != sentinel {
		t.Errorf("error = %v, want exactly %v (wrapper must not wrap)", err, sentinel)
	}
}
