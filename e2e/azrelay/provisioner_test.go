package azrelay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

func TestHycoNamePattern(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"valid entra", "e2e-entra-0123456789ab", true},
		{"valid sas", "e2e-sas-abcdef012345", true},
		{"static entra rejected", "e2e-entra", false},
		{"static sas rejected", "e2e-sas", false},
		{"short suffix", "e2e-entra-0123456789a", false},
		{"long suffix", "e2e-entra-0123456789abc", false},
		{"uppercase suffix", "e2e-entra-0123456789AB", false},
		{"non-hex suffix", "e2e-entra-0123456789xy", false},
		{"wrong middle", "e2e-foo-0123456789ab", false},
		{"unrelated name", "production-hyco", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HycoNamePattern.MatchString(tc.in)
			if got != tc.want {
				t.Errorf("HycoNamePattern.MatchString(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewSuffix(t *testing.T) {
	const want = suffixLen
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		s, err := newSuffix()
		if err != nil {
			t.Fatalf("newSuffix: %v", err)
		}
		if len(s) != want {
			t.Fatalf("newSuffix length = %d, want %d", len(s), want)
		}
		if strings.ToLower(s) != s {
			t.Fatalf("newSuffix not lowercase: %q", s)
		}
		for _, c := range s {
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
			if !isHex {
				t.Fatalf("newSuffix has non-hex char %q in %q", c, s)
			}
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("newSuffix collision after 64 iterations: %q", s)
		}
		seen[s] = struct{}{}
		// Verify the generated suffix produces hyco names that the
		// janitor's regex will accept.
		if !HycoNamePattern.MatchString("e2e-entra-" + s) {
			t.Fatalf("generated suffix %q does not satisfy HycoNamePattern", s)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "missing subscription",
			cfg:     Config{ResourceGroup: "rg", Namespace: "ns"},
			wantErr: "SubscriptionID",
		},
		{
			name:    "missing resource group",
			cfg:     Config{SubscriptionID: "sub", Namespace: "ns"},
			wantErr: "ResourceGroup",
		},
		{
			name:    "missing namespace",
			cfg:     Config{SubscriptionID: "sub", ResourceGroup: "rg"},
			wantErr: "Namespace",
		},
		{
			name: "all set",
			cfg:  Config{SubscriptionID: "sub", ResourceGroup: "rg", Namespace: "ns"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate: unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate: expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate: error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestResultEnvVars(t *testing.T) {
	r := &Result{
		RelayName:       "my-relay",
		EntraHycoName:   "e2e-entra-0123456789ab",
		SASHycoName:     "e2e-sas-0123456789ab",
		ListenerKeyName: "listener",
		ListenerKey:     "lk",
		SenderKeyName:   "sender",
		SenderKey:       "sk",
	}
	want := map[string]string{
		"E2E_RELAY_NAME":            "my-relay",
		"E2E_ENTRA_HYCO_NAME":       "e2e-entra-0123456789ab",
		"E2E_SAS_HYCO_NAME":         "e2e-sas-0123456789ab",
		"E2E_SAS_LISTENER_KEY_NAME": "listener",
		"E2E_SAS_LISTENER_KEY":      "lk",
		"E2E_SAS_SENDER_KEY_NAME":   "sender",
		"E2E_SAS_SENDER_KEY":        "sk",
	}
	got := r.EnvVars()
	if len(got) != len(want) {
		t.Fatalf("EnvVars: len = %d, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("EnvVars[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestProvisionerSingleUse(t *testing.T) {
	// A Provisioner is single-use: a second Provision call must fail
	// without contacting Azure. Construct one with a sentinel Result
	// already populated and confirm Provision returns the expected error.
	p := &Provisioner{result: &Result{}}
	if _, err := p.Provision(t.Context()); err == nil {
		t.Fatal("expected error on reuse")
	} else if !strings.Contains(err.Error(), "already used") {
		t.Fatalf("error %q does not mention reuse", err.Error())
	}
}

func TestProvisionerHycoNamesUsesSuffix(t *testing.T) {
	p := &Provisioner{suffix: "0123456789ab"}
	entra, sas := p.HycoNames()
	if entra != "e2e-entra-0123456789ab" {
		t.Errorf("entra name = %q", entra)
	}
	if sas != "e2e-sas-0123456789ab" {
		t.Errorf("sas name = %q", sas)
	}
	for _, n := range []string{entra, sas} {
		if !HycoNamePattern.MatchString(n) {
			t.Errorf("generated name %q does not satisfy HycoNamePattern", n)
		}
	}
}

// fastAuthRuleRetry returns a retry config that runs the same number of
// attempts as production but with near-zero delays so unit tests don't
// pay the 500ms…8s backoff schedule. Behaviour-equivalent for the loop's
// control flow (success/error/exhaust/cancel) and jitter is bounded by
// initialDelay so even at full jitter the test budget is microseconds.
func fastAuthRuleRetry() authRuleRetry {
	return authRuleRetry{
		maxAttempts:  authRuleMaxAttempts,
		initialDelay: time.Microsecond,
		maxDelay:     10 * time.Microsecond,
	}
}

// newMessagingGatewayErr builds a *azcore.ResponseError that mirrors the
// shape Azure Relay returns for a 429 MessagingGatewayTooManyRequests:
// the StatusCode/ErrorCode fields plus a RawResponse whose body carries
// the SubCode marker that azcore.Error() embeds in its rendered string.
// The SubCode the caller specifies is what isAuthRuleConflict will see.
func newMessagingGatewayErr(subCode int) error {
	body := fmt.Sprintf(
		`{"error":{"code":"MessagingGatewayTooManyRequests","message":"SubCode=%d. Another conflicting operation is in progress."}}`,
		subCode,
	)
	req, _ := http.NewRequest(http.MethodPut, "http://example.com/rule", nil)
	return &azcore.ResponseError{
		StatusCode: http.StatusTooManyRequests,
		ErrorCode:  "MessagingGatewayTooManyRequests",
		RawResponse: &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Body:       io.NopCloser(bytes.NewBufferString(body)),
			Header:     http.Header{},
			Request:    req,
		},
	}
}

// newConflictErr returns the 40901 SubCode shape isAuthRuleConflict
// recognises (the exact failure mode the retry exists to absorb).
func newConflictErr() error { return newMessagingGatewayErr(40901) }

func TestIsAuthRuleConflict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "non-ResponseError",
			err:  errors.New("plain error"),
			want: false,
		},
		{
			name: "matches 429 + MessagingGatewayTooManyRequests + SubCode=40901",
			err:  newConflictErr(),
			want: true,
		},
		{
			name: "wrapped match still recognised",
			err:  fmt.Errorf("create rule: %w", newConflictErr()),
			want: true,
		},
		{
			name: "same ErrorCode but different SubCode is not retried",
			err:  newMessagingGatewayErr(40902),
			want: false,
		},
		{
			name: "same ErrorCode with no SubCode marker is not retried",
			err: &azcore.ResponseError{
				StatusCode: http.StatusTooManyRequests,
				ErrorCode:  "MessagingGatewayTooManyRequests",
			},
			want: false,
		},
		{
			name: "429 with different ErrorCode is not retried",
			err: &azcore.ResponseError{
				StatusCode: http.StatusTooManyRequests,
				ErrorCode:  "SomeOtherThrottling",
			},
			want: false,
		},
		{
			name: "429 with empty ErrorCode is not retried",
			err: &azcore.ResponseError{
				StatusCode: http.StatusTooManyRequests,
			},
			want: false,
		},
		{
			name: "503 with matching ErrorCode + SubCode is not retried (wrong status)",
			err: &azcore.ResponseError{
				StatusCode: http.StatusServiceUnavailable,
				ErrorCode:  "MessagingGatewayTooManyRequests",
			},
			want: false,
		},
		{
			name: "401 is not retried",
			err: &azcore.ResponseError{
				StatusCode: http.StatusUnauthorized,
				ErrorCode:  "MessagingGatewayTooManyRequests",
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAuthRuleConflict(tc.err); got != tc.want {
				t.Fatalf("isAuthRuleConflict(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRetryOnAuthRuleConflict_SuccessFirstAttempt(t *testing.T) {
	calls := 0
	err := retryOnAuthRuleConflict(t.Context(), fastAuthRuleRetry(), func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestRetryOnAuthRuleConflict_RetriesThenSucceeds(t *testing.T) {
	calls := 0
	const succeedOn = 4
	err := retryOnAuthRuleConflict(t.Context(), fastAuthRuleRetry(), func() error {
		calls++
		if calls < succeedOn {
			return newConflictErr()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != succeedOn {
		t.Fatalf("calls = %d, want %d", calls, succeedOn)
	}
}

func TestRetryOnAuthRuleConflict_NonConflictReturnsImmediately(t *testing.T) {
	calls := 0
	sentinel := errors.New("hard failure")
	err := retryOnAuthRuleConflict(t.Context(), fastAuthRuleRetry(), func() error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (non-conflict must not retry)", calls)
	}
}

// A 429 with a different ErrorCode (e.g. SubscriptionThrottle) must not be
// retried by this loop — the retry contract is explicit: only the
// specific 40901 class is retried.
func TestRetryOnAuthRuleConflict_Generic429NotRetried(t *testing.T) {
	calls := 0
	generic := &azcore.ResponseError{
		StatusCode: http.StatusTooManyRequests,
		ErrorCode:  "SubscriptionRequestThrottled",
	}
	err := retryOnAuthRuleConflict(t.Context(), fastAuthRuleRetry(), func() error {
		calls++
		return generic
	})
	if !errors.Is(err, generic) {
		t.Fatalf("err = %v, want %v", err, generic)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (generic 429 must not retry)", calls)
	}
}

func TestRetryOnAuthRuleConflict_ExhaustWrapsLastErr(t *testing.T) {
	calls := 0
	last := newConflictErr()
	err := retryOnAuthRuleConflict(t.Context(), fastAuthRuleRetry(), func() error {
		calls++
		return last
	})
	if err == nil {
		t.Fatal("expected error after exhausting attempts, got nil")
	}
	if !errors.Is(err, last) {
		t.Fatalf("err %v does not wrap last %v", err, last)
	}
	if !strings.Contains(err.Error(), "after") {
		t.Fatalf("err %q does not mention attempts", err.Error())
	}
	if calls != authRuleMaxAttempts {
		t.Fatalf("calls = %d, want %d", calls, authRuleMaxAttempts)
	}
}

func TestRetryOnAuthRuleConflict_ContextCancelledBetweenAttempts(t *testing.T) {
	// Drive the cancellation deterministically from inside fn so the test
	// doesn't depend on scheduler wall-clock timing: the first attempt
	// cancels ctx and returns a retriable conflict; the loop's select
	// then observes ctx.Done immediately and returns context.Canceled
	// without a second fn call.
	cfg := authRuleRetry{
		maxAttempts:  authRuleMaxAttempts,
		initialDelay: time.Second, // never actually waited
		maxDelay:     time.Second,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	err := retryOnAuthRuleConflict(ctx, cfg, func() error {
		calls++
		cancel()
		return newConflictErr()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (cancel must abort before second attempt)", calls)
	}
}

func TestJitterBounds(t *testing.T) {
	// jitter(d) must lie in [d/2, d] for positive d, and be 0 for d <= 0.
	if got := jitter(0); got != 0 {
		t.Fatalf("jitter(0) = %v, want 0", got)
	}
	if got := jitter(-time.Second); got != 0 {
		t.Fatalf("jitter(-1s) = %v, want 0", got)
	}
	for range 256 {
		d := 4 * time.Millisecond
		got := jitter(d)
		if got < d/2 || got > d {
			t.Fatalf("jitter(%v) = %v outside [%v, %v]", d, got, d/2, d)
		}
	}
}
