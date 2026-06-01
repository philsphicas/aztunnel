package scenarios

import (
	"testing"
)

// TestBackendScope_AppliesTo exercises the scope predicate against
// the two backend names the harness actually uses ("mock", "azure")
// plus a defensive case for an unknown name. Out-of-range scope
// values must return false rather than panic so a registry bug
// surfaces as a missed case, not a runtime crash.
func TestBackendScope_AppliesTo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		scope BackendScope
		name  string
		want  bool
	}{
		{AnyBackend, "mock", true},
		{AnyBackend, "azure", true},
		{AnyBackend, "future", true},
		{MockOnly, "mock", true},
		{MockOnly, "azure", false},
		{MockOnly, "future", false},
		{AzureOnly, "azure", true},
		{AzureOnly, "mock", false},
		{AzureOnly, "future", false},
		{BackendScope(99), "mock", false},
		{BackendScope(99), "azure", false},
	}
	for _, tc := range cases {
		got := tc.scope.appliesTo(tc.name)
		if got != tc.want {
			t.Errorf("BackendScope(%v).appliesTo(%q) = %v, want %v", tc.scope, tc.name, got, tc.want)
		}
	}
}

// TestBackendScope_String checks the human-readable rendering used
// in skip messages. The default branch is required: array-indexed
// rendering panics on invalid values, which would convert a registry
// bug into an obscure crash inside b.Skipf rather than a visible
// "unknown(N)" string.
func TestBackendScope_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		scope BackendScope
		want  string
	}{
		{AnyBackend, "any"},
		{MockOnly, "mock-only"},
		{AzureOnly, "azure-only"},
		{BackendScope(99), "unknown(99)"},
	}
	for _, tc := range cases {
		if got := tc.scope.String(); got != tc.want {
			t.Errorf("BackendScope(%v).String() = %q, want %q", int(tc.scope), got, tc.want)
		}
	}
}
