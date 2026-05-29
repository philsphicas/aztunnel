package scenarios

import (
	"strings"
	"testing"
)

// TestBackendScope_AppliesTo exercises the scope predicate against
// the two backend names the harness actually uses ("mock", "azure")
// plus a defensive case for an unknown name. Out-of-range scope
// values must return false rather than panic so a registry bug
// surfaces as a missed sub-bench, not a runtime crash.
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

// TestBenchmarkCases_Registry pins the registry shape so a stray
// edit (deletion, duplicate name, missing reason on a scoped entry)
// surfaces as a unit-test failure rather than as a silently-missing
// sub-bench in the bench output.
func TestBenchmarkCases_Registry(t *testing.T) {
	t.Parallel()
	cases := benchmarkCases()
	if len(cases) == 0 {
		t.Fatal("benchmarkCases() returned no entries")
	}

	seen := make(map[string]struct{}, len(cases))
	for _, bc := range cases {
		if bc.name == "" {
			t.Errorf("benchmarkCases() has an entry with empty name")
			continue
		}
		if _, dup := seen[bc.name]; dup {
			t.Errorf("benchmarkCases() has duplicate entry %q", bc.name)
		}
		seen[bc.name] = struct{}{}
		if bc.run == nil {
			t.Errorf("benchmarkCases()[%q].run is nil", bc.name)
		}
		// A non-AnyBackend entry will be skipped on at least one
		// backend; the reason is what the user sees in that skip
		// message. An empty reason means a silent "skipped — figure
		// it out yourself" — disallow at the registry layer.
		if bc.scope != AnyBackend && strings.TrimSpace(bc.reason) == "" {
			t.Errorf("benchmarkCases()[%q] has scope=%s but empty reason", bc.name, bc.scope)
		}
	}
}

// TestBenchmarkCases_BackendCounts asserts the post-cleanup
// distribution: the mock backend runs every registered entry and the
// Azure backend runs every entry except those explicitly scoped to
// MockOnly. If the counts shift, this test fails loudly and the
// PR's bench surface change is forced through review.
func TestBenchmarkCases_BackendCounts(t *testing.T) {
	t.Parallel()
	cases := benchmarkCases()

	var mockRuns, mockSkips int
	var azureRuns, azureSkips int
	for _, bc := range cases {
		if bc.scope.appliesTo("mock") {
			mockRuns++
		} else {
			mockSkips++
		}
		if bc.scope.appliesTo("azure") {
			azureRuns++
		} else {
			azureSkips++
		}
	}

	// Expected after the Option-3 cleanup: 4 AnyBackend + 1 MockOnly
	// + 0 AzureOnly.
	const wantMockRuns = 5
	const wantAzureRuns = 4
	if mockRuns != wantMockRuns || mockSkips != 0 {
		t.Errorf("mock backend: runs=%d skips=%d, want runs=%d skips=0", mockRuns, mockSkips, wantMockRuns)
	}
	if azureRuns != wantAzureRuns || azureSkips != 1 {
		t.Errorf("azure backend: runs=%d skips=%d, want runs=%d skips=1", azureRuns, azureSkips, wantAzureRuns)
	}
}

// TestBenchmarkCases_ConcurrentConnectMockOnly pins the scope
// assignment for ConcurrentConnect_N100 specifically: the user
// decision in PR #101 was that on Azure this bench is dominated by
// gateway-contention noise, so it runs only against the mock. If a
// future change demotes the scope, this test fails and the reviewer
// has to either justify the change in writing or update this test
// — both of which surface the surface-area regression.
func TestBenchmarkCases_ConcurrentConnectMockOnly(t *testing.T) {
	t.Parallel()
	const want = "ConcurrentConnect_N100"
	for _, bc := range benchmarkCases() {
		if bc.name != want {
			continue
		}
		if bc.scope != MockOnly {
			t.Errorf("%s scope = %s, want %s", want, bc.scope, MockOnly)
		}
		if !strings.Contains(bc.reason, "gateway-contention") {
			t.Errorf("%s reason should explain Azure noise but is %q", want, bc.reason)
		}
		return
	}
	t.Fatalf("%s entry not found in benchmarkCases()", want)
}
