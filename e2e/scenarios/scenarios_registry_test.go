package scenarios

import (
	"strings"
	"testing"
)

// TestScenarioCases_RegistryShape asserts every Run*Scenarios
// registry passes the same invariants as the bench registry:
//
//   - non-empty
//   - no duplicate names within a registry
//   - every entry has a non-nil run closure
//   - non-AnyBackend entries carry a non-empty reason (the operator
//     reads it from the t.Skipf in test output)
//
// This is the unit-testable shape check that lets contributors edit
// the registry without standing up a topology.
func TestScenarioCases_RegistryShape(t *testing.T) {
	t.Parallel()
	suites := map[string][]scenarioCase{
		"core":          coreCases(),
		"reliability":   reliabilityCases(),
		"observability": observabilityCases(),
		"performance":   performanceCases(),
		"topology":      topologyCases(),
	}
	for suite, cases := range suites {
		suite, cases := suite, cases
		t.Run(suite, func(t *testing.T) {
			t.Parallel()
			if len(cases) == 0 {
				t.Fatalf("%sCases() returned no entries", suite)
			}
			seen := make(map[string]struct{}, len(cases))
			for _, sc := range cases {
				if sc.name == "" {
					t.Errorf("%sCases() has an entry with empty name", suite)
					continue
				}
				if _, dup := seen[sc.name]; dup {
					t.Errorf("%sCases() has duplicate entry %q", suite, sc.name)
				}
				seen[sc.name] = struct{}{}
				if sc.run == nil {
					t.Errorf("%sCases()[%q].run is nil", suite, sc.name)
				}
				if sc.scope != AnyBackend && strings.TrimSpace(sc.reason) == "" {
					t.Errorf("%sCases()[%q] has scope=%s but empty reason", suite, sc.name, sc.scope)
				}
			}
		})
	}
}

// TestScenarioCases_AzureOnlyScopes pins the set of scenarios that
// carry scope=AzureOnly. Demoting any of these to AnyBackend would
// silently break the cross-backend parity contract (the mock would
// suddenly be asked to honor an Azure-only shape it cannot
// emulate); promoting a new entry to AzureOnly without justification
// would silently shrink the mock-side coverage matrix. Both
// directions fail the test, forcing the change through review.
func TestScenarioCases_AzureOnlyScopes(t *testing.T) {
	t.Parallel()

	wantAzureOnly := map[string]string{
		// reliability
		"AuthRejection_BadHyco": "reliability",
		"LongLivedConnection":   "reliability",
		// observability
		"TokenFetchMetric": "observability",
	}

	gotAzureOnly := map[string]string{}
	suites := map[string][]scenarioCase{
		"core":          coreCases(),
		"reliability":   reliabilityCases(),
		"observability": observabilityCases(),
		"performance":   performanceCases(),
		"topology":      topologyCases(),
	}
	for suite, cases := range suites {
		for _, sc := range cases {
			if sc.scope == AzureOnly {
				gotAzureOnly[sc.name] = suite
			}
		}
	}

	for name, wantSuite := range wantAzureOnly {
		gotSuite, ok := gotAzureOnly[name]
		if !ok {
			t.Errorf("scope=AzureOnly entry %q missing from registries (expected in %s)", name, wantSuite)
			continue
		}
		if gotSuite != wantSuite {
			t.Errorf("scope=AzureOnly entry %q in suite=%q, want suite=%q", name, gotSuite, wantSuite)
		}
	}
	for name, gotSuite := range gotAzureOnly {
		if _, ok := wantAzureOnly[name]; !ok {
			t.Errorf("unexpected scope=AzureOnly entry %q in suite=%q — add to pinned list or restore to AnyBackend",
				name, gotSuite)
		}
	}
}

// TestScenarioCases_MockOnlyScopes pins the set of scenarios that
// carry scope=MockOnly. None today: every scenario either runs on
// both backends (AnyBackend) or is Azure-specific (AzureOnly).
// MockOnly is reserved for cases whose signal depends on having no
// real network/data-plane noise; adding one here without
// justification should force this test to fail.
func TestScenarioCases_MockOnlyScopes(t *testing.T) {
	t.Parallel()
	for suite, cases := range map[string][]scenarioCase{
		"core":          coreCases(),
		"reliability":   reliabilityCases(),
		"observability": observabilityCases(),
		"performance":   performanceCases(),
		"topology":      topologyCases(),
	} {
		for _, sc := range cases {
			if sc.scope == MockOnly {
				t.Errorf("unexpected scope=MockOnly scenario %q in suite=%q — bench cases may be MockOnly (see benchmarkCases()) but scenarios should be either AnyBackend or AzureOnly",
					sc.name, suite)
			}
		}
	}
}
