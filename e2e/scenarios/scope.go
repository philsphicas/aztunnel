package scenarios

import (
	"fmt"
	"testing"
)

// BackendScope restricts a registered case (scenario or benchmark) to
// a subset of backends. Out-of-scope cases are skipped via t.Skipf /
// b.Skipf at registration time so the scope decision is visible in
// the test output rather than hidden in an inline `if b.Name() != "x"`
// inside the case body.
//
// The same enum drives the bench registry (benchCase in bench.go) and
// the scenario registries (scenarioCase below, used by Run*Scenarios).
type BackendScope int

const (
	// AnyBackend runs the case on every backend.
	AnyBackend BackendScope = iota
	// MockOnly runs the case only when the backend is the in-process
	// mock. Use when the case's signal depends on having no network
	// jitter or shared-namespace contention — the mock isolates
	// aztunnel CPU/goroutine cost cleanly, while the same case on
	// Azure would be dominated by data-plane variance.
	MockOnly
	// AzureOnly runs the case only when the backend is the real
	// Azure Relay. Use for cases whose signal only exists on the
	// real data plane (e.g. namespace-side idle timeout, real
	// Entra/SAS provider end-to-end metric wiring).
	AzureOnly
)

func (s BackendScope) appliesTo(backendName string) bool {
	switch s {
	case AnyBackend:
		return true
	case MockOnly:
		return backendName == "mock"
	case AzureOnly:
		return backendName == "azure"
	default:
		return false
	}
}

func (s BackendScope) String() string {
	switch s {
	case AnyBackend:
		return "any"
	case MockOnly:
		return "mock-only"
	case AzureOnly:
		return "azure-only"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// scenarioCase is one entry in a scenario suite registry (Core,
// Reliability, Observability, Performance, Topology). The registries
// are pure metadata: no scenario body runs and no Backend is
// referenced when xxxCases() is called, so each registry can be
// asserted in unit tests without standing up a topology.
//
// scope restricts which backends run this case; out-of-scope
// invocations are skipped via t.Skipf with `reason` rendered.
// reason is required for non-AnyBackend scopes; ignored otherwise
// (the bench/scenario registry pin tests enforce both invariants).
type scenarioCase struct {
	name   string
	scope  BackendScope
	reason string
	run    func(*testing.T, Backend)
}

// runScenarioCases is the shared driver for every Run*Scenarios
// entry point. Each case becomes a sub-test of t; scope-excluded
// cases are emitted as t.Skipf("scope=…, backend=…: …") so the
// skip is visible in test output.
func runScenarioCases(t *testing.T, b Backend, cases []scenarioCase) {
	t.Helper()
	name := b.Name()
	for _, sc := range cases {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			if !sc.scope.appliesTo(name) {
				t.Skipf("scope=%s, backend=%q: %s", sc.scope, name, sc.reason)
				return
			}
			sc.run(t, b)
		})
	}
}
