package azrelay

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

func TestPermanentRuleNamesAreStable(t *testing.T) {
	// Pin the permanent rule names against accidental rename. The
	// names are baked into operational tooling (`e2e-infra setup`
	// provisions them; `az relay namespace authorization-rule …`
	// is how maintainers manage them out-of-band), so changing
	// them silently would orphan the existing rules and break
	// every CI run.
	if PermanentListenerRuleName != "e2e-listener" {
		t.Errorf("PermanentListenerRuleName = %q, want %q (orphans the rule in every provisioned namespace if changed)",
			PermanentListenerRuleName, "e2e-listener")
	}
	if PermanentSenderRuleName != "e2e-sender" {
		t.Errorf("PermanentSenderRuleName = %q, want %q (orphans the rule in every provisioned namespace if changed)",
			PermanentSenderRuleName, "e2e-sender")
	}
}

func TestPermanentRulesAreDistinctFromHycoPattern(t *testing.T) {
	// The janitor sweeps hycos by HycoNamePattern. If a permanent
	// rule name ever drifted into the hyco regex's match space, the
	// janitor would attempt to delete the rule as a hyco — wrong
	// resource type, but a regression worth pinning anyway.
	for _, name := range []string{PermanentListenerRuleName, PermanentSenderRuleName} {
		if HycoNamePattern.MatchString(name) {
			t.Errorf("HycoNamePattern must not match permanent rule name %q", name)
		}
	}
}

func TestIsNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"404 ResponseError", &azcore.ResponseError{StatusCode: http.StatusNotFound}, true},
		{"wrapped 404", &wrapErr{inner: &azcore.ResponseError{StatusCode: http.StatusNotFound}}, true},
		{"403", &azcore.ResponseError{StatusCode: http.StatusForbidden}, false},
		{"500", &azcore.ResponseError{StatusCode: http.StatusInternalServerError}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNotFound(tc.err); got != tc.want {
				t.Errorf("isNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

type wrapErr struct{ inner error }

func (w *wrapErr) Error() string { return "wrap: " + w.inner.Error() }
func (w *wrapErr) Unwrap() error { return w.inner }

func TestValidateForRunRules_MirrorsValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"missing subscription", Config{ResourceGroup: "rg", Namespace: "ns"}, "SubscriptionID"},
		{"missing resource group", Config{SubscriptionID: "sub", Namespace: "ns"}, "ResourceGroup"},
		{"missing namespace", Config{SubscriptionID: "sub", ResourceGroup: "rg"}, "Namespace"},
		{"all set", Config{SubscriptionID: "sub", ResourceGroup: "rg", Namespace: "ns"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validateForRunRules()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateForRunRules: unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateForRunRules: err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestTeardownIsNoOp(t *testing.T) {
	// RunRules.Teardown is a no-op because the permanent rules are
	// owned by `e2e-infra setup`. Pin that contract so a future
	// refactor doesn't silently reintroduce delete-on-teardown —
	// which would race every other in-flight CI run sharing the
	// namespace.
	rr := &RunRules{
		ListenerName: PermanentListenerRuleName,
		ListenerKey:  "listener-key",
		SenderName:   PermanentSenderRuleName,
		SenderKey:    "sender-key",
	}
	if err := rr.Teardown(t.Context()); err != nil {
		t.Fatalf("Teardown should be a no-op, got error: %v", err)
	}
	// Idempotent — multiple calls remain safe.
	if err := rr.Teardown(t.Context()); err != nil {
		t.Fatalf("Teardown second call should still be a no-op, got error: %v", err)
	}
}

// stubRunRules returns a non-nil *RunRules suitable only for satisfying
// the Config.RunRules-required check in NewProvider for unit tests that
// never reach the actual ARM rule-mutation path. Production callers
// must go through AcquireRunRules.
func stubRunRules() *RunRules {
	return &RunRules{
		ListenerName: PermanentListenerRuleName,
		ListenerKey:  "stub-listener-key",
		SenderName:   PermanentSenderRuleName,
		SenderKey:    "stub-sender-key",
	}
}
