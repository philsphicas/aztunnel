//go:build e2e

package mock_test

import (
	"os"
	"strings"
	"testing"

	"github.com/philsphicas/aztunnel/e2e/backends/mock"
	"github.com/philsphicas/aztunnel/mockrelay/server"
)

// mockBackendFromEnv builds the MockBackend for the e2e scenario run
// from the E2E_AUTH and E2E_DELAY environment variables, mirroring the
// knobs the Azure backend uses to vary its matrix. The two dimensions
// are independent and compose: the suite runs once per (auth, delay)
// cell.
//
// A dimension with a single selected value is pinned with no axis, so
// the common `make e2e-mock` sub-test paths are unchanged and `-run`
// selectors keep matching; a dimension with two or more values adds a
// sub-test layer (auth outermost, then delay), e.g.
// TestE2E_Mock/entra/default/Performance/... Unknown values fail the
// test loudly.
func mockBackendFromEnv(t testing.TB) *mock.MockBackend {
	t.Helper()
	return mock.NewMatrixBackend(authNamesFromEnv(t), delayProfileNamesFromEnv(t))
}

// authNamesFromEnv parses E2E_AUTH into the ordered set of auth methods
// to run, mirroring the Azure backend's E2E_AUTH knob:
//
//   - unset / empty -> both {sas, entra} (the default matrix).
//   - "sas" / "entra" -> that single method, pinned (no auth axis).
//
// Any other value fails the test loudly.
func authNamesFromEnv(t testing.TB) []string {
	t.Helper()
	switch raw := strings.TrimSpace(os.Getenv("E2E_AUTH")); raw {
	case "":
		return []string{mock.AuthSAS, mock.AuthEntra}
	case mock.AuthSAS, mock.AuthEntra:
		return []string{raw}
	default:
		t.Fatalf("E2E_AUTH=%q: want %q, %q, or unset (both)", raw, mock.AuthSAS, mock.AuthEntra)
		return nil
	}
}

// delayProfileNamesFromEnv parses E2E_DELAY into the ordered, deduped
// set of profile names to run, mirroring the E2E_AUTH knob:
//
//   - unset / empty -> the single "default" profile (the wire-faithful
//     profile the e2e timing thresholds are calibrated against).
//   - "all"         -> every registered profile, in sorted order.
//   - "a,b,c"       -> the listed profiles, in the given order, deduped.
//
// Every name is validated against the mockrelay registry; an empty
// element or unknown name fails the test.
func delayProfileNamesFromEnv(t testing.TB) []string {
	t.Helper()
	raw := os.Getenv("E2E_DELAY")
	switch raw {
	case "":
		return []string{"default"}
	case "all":
		return server.ProfileNames()
	}
	seen := make(map[string]struct{})
	var names []string
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			t.Fatalf("E2E_DELAY: empty profile name in %q", raw)
		}
		if _, err := server.ProfileByName(name); err != nil {
			t.Fatalf("E2E_DELAY: %v", err)
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}
