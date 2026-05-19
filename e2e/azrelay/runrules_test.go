package azrelay

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

func TestRunRuleNamePattern(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"valid listener", "e2e-run-0123456789ab-listener", true},
		{"valid sender", "e2e-run-abcdef012345-sender", true},
		{"missing role", "e2e-run-0123456789ab", false},
		{"unknown role", "e2e-run-0123456789ab-admin", false},
		{"short suffix", "e2e-run-0123456789a-listener", false},
		{"long suffix", "e2e-run-0123456789abc-listener", false},
		{"uppercase suffix", "e2e-run-0123456789AB-listener", false},
		{"non-hex suffix", "e2e-run-0123456789xy-listener", false},
		{"uppercase role", "e2e-run-0123456789ab-LISTENER", false},
		{"wrong prefix", "e2e-rule-0123456789ab-listener", false},
		{"unrelated name", "production-rule", false},
		{"empty", "", false},
		// Hyco names must NOT accidentally match.
		{"hyco entra rejected", "e2e-entra-0123456789ab", false},
		{"hyco sas rejected", "e2e-sas-0123456789ab", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RunRuleNamePattern.MatchString(tc.in); got != tc.want {
				t.Errorf("RunRuleNamePattern.MatchString(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestRunRuleAndHycoPatternsAreDisjoint(t *testing.T) {
	// Janitor relies on the two sweeps owning disjoint name spaces;
	// regression-test the pair so a future rename can't silently
	// cause one sweep to delete entities the other owns.
	const suffix = "0123456789ab"
	listener := "e2e-run-" + suffix + "-listener"
	sender := "e2e-run-" + suffix + "-sender"
	entra := "e2e-entra-" + suffix
	sas := "e2e-sas-" + suffix

	if !RunRuleNamePattern.MatchString(listener) || !RunRuleNamePattern.MatchString(sender) {
		t.Fatalf("RunRuleNamePattern must match its own outputs (%s, %s)", listener, sender)
	}
	if HycoNamePattern.MatchString(listener) || HycoNamePattern.MatchString(sender) {
		t.Fatalf("HycoNamePattern must not match run-rule names (%s, %s)", listener, sender)
	}
	if !HycoNamePattern.MatchString(entra) || !HycoNamePattern.MatchString(sas) {
		t.Fatalf("HycoNamePattern must match its own outputs (%s, %s)", entra, sas)
	}
	if RunRuleNamePattern.MatchString(entra) || RunRuleNamePattern.MatchString(sas) {
		t.Fatalf("RunRuleNamePattern must not match hyco names (%s, %s)", entra, sas)
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

// stubRunRules returns a non-nil *RunRules suitable only for satisfying
// the Config.RunRules-required check in NewProvider for unit tests that
// never reach the actual ARM rule-mutation path. Production callers
// must go through AcquireRunRules.
func stubRunRules() *RunRules {
	return &RunRules{
		ListenerName: "e2e-run-stubstubstub-listener",
		ListenerKey:  "stub-listener-key",
		SenderName:   "e2e-run-stubstubstub-sender",
		SenderKey:    "stub-sender-key",
	}
}
