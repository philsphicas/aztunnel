//go:build e2e

package mock_test

import (
	"bytes"
	"io"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/e2e/backends/mock"
	"github.com/philsphicas/aztunnel/e2e/scenarios"
	"github.com/philsphicas/aztunnel/mockrelay/server"
)

// TestMockFeature_DefaultDelayProfile verifies that a MockBackend
// configured with server.DelayProfileDefault applies the wire-faithful
// per-step delays to each rendezvous, observable as a lower bound on
// the elapsed time for a single sender → listener → target round-
// trip.
//
// We assert a generous lower bound that survives jitter while still
// tripping any regression that silently disables the delay sleeps.
func TestMockFeature_DefaultDelayProfile(t *testing.T) {
	b := mock.MockBackend{DelayProfile: server.DelayProfileDefault}
	elapsed := timeOneRoundTrip(t, &b)
	wantAtLeast := 300 * time.Millisecond
	if elapsed < wantAtLeast {
		t.Fatalf("round-trip took %v with DelayProfileDefault; want >= %v",
			elapsed, wantAtLeast)
	}
}

// TestMockFeature_ZeroDelayProfile verifies the zero-value
// MockBackend{} (zero DelayProfile) applies no synthetic per-step
// delay, leaving rendezvous + bridge bounded by the in-process
// baseline (single-digit ms).
//
// Upper bound is 500 ms — well below the DelayProfileDefault wall-clock
// so any accidental fall-through to a non-zero profile trips the test.
func TestMockFeature_ZeroDelayProfile(t *testing.T) {
	var b mock.MockBackend
	elapsed := timeOneRoundTrip(t, &b)
	upperBound := 500 * time.Millisecond
	if elapsed > upperBound {
		t.Fatalf("round-trip took %v with zero DelayProfile; want < %v",
			elapsed, upperBound)
	}
}

// TestMockFeature_DelayMatrix verifies the matrix wiring a
// NewMatrixBackend exposes to the harness when only the delay
// dimension varies: a single "delay" axis whose values are the
// requested profiles, a Cell() that pins each profile and reports no
// further axes, and a per-profile latency threshold that scales above
// the floor for a slow profile. It stands up no topology — it exercises
// the Backend matrix contract directly.
func TestMockFeature_DelayMatrix(t *testing.T) {
	b := mock.NewMatrixBackend([]string{mock.AuthSAS}, []string{"zero", "default"})

	axes := b.Axes()
	if len(axes) != 1 || axes[0].Name() != "delay" {
		t.Fatalf("expected a single \"delay\" axis, got %v", axes)
	}
	if got := axes[0].Values(); !slices.Equal(got, []string{"zero", "default"}) {
		t.Fatalf("axis values = %v, want [zero default]", got)
	}

	// Each cell pins the named profile and advertises no further axes.
	def := b.Cell(map[string]string{"delay": "default"})
	pinned, ok := def.(*mock.MockBackend)
	if !ok {
		t.Fatalf("Cell returned %T, want *mock.MockBackend", def)
	}
	if pinned.Axes() != nil {
		t.Errorf("pinned cell backend should report nil Axes(), got %v", pinned.Axes())
	}

	// A slow profile's derived threshold scales above the 3 s floor,
	// proving the budget tracks the profile rather than a flat constant.
	slow := mock.MockBackend{DelayProfile: server.DelayProfile{
		SLatency: 2 * time.Second,
		LLatency: 2 * time.Second,
	}}
	if got := slow.ConnectLatencyThreshold(); got <= 3*time.Second {
		t.Errorf("slow-profile ConnectLatencyThreshold = %v, want > 3s (budget should scale)", got)
	}
}

// TestMockFeature_AuthDelayMatrix verifies that NewMatrixBackend
// composes the auth and delay dimensions: when both vary it advertises
// two axes in the order [auth, delay] (auth outermost, mirroring the
// Azure backend), Cell() pins both from the supplied keys, and a
// single-valued dimension is pinned with no axis. It also checks the
// entra cold-start budget carries headroom over the SAS budget when the
// profile models a token-acquisition cost, and collapses to equality
// under the zero profile.
func TestMockFeature_AuthDelayMatrix(t *testing.T) {
	b := mock.NewMatrixBackend([]string{mock.AuthSAS, mock.AuthEntra}, []string{"zero", "default"})

	axes := b.Axes()
	if len(axes) != 2 || axes[0].Name() != "auth" || axes[1].Name() != "delay" {
		t.Fatalf("expected axes [auth delay], got %v", axes)
	}
	if got := axes[0].Values(); !slices.Equal(got, []string{mock.AuthSAS, mock.AuthEntra}) {
		t.Fatalf("auth axis values = %v, want [sas entra]", got)
	}

	// A fully specified cell pins both dimensions and advertises none.
	cell := b.Cell(map[string]string{"auth": mock.AuthEntra, "delay": "default"})
	pinned, ok := cell.(*mock.MockBackend)
	if !ok {
		t.Fatalf("Cell returned %T, want *mock.MockBackend", cell)
	}
	if pinned.Axes() != nil {
		t.Errorf("pinned cell should report nil Axes(), got %v", pinned.Axes())
	}

	// A single-valued auth dimension is pinned with no axis; only the
	// delay axis remains.
	delayOnly := mock.NewMatrixBackend([]string{mock.AuthEntra}, []string{"zero", "default"})
	if axes := delayOnly.Axes(); len(axes) != 1 || axes[0].Name() != "delay" {
		t.Fatalf("single-auth backend axes = %v, want [delay] only", axes)
	}

	// Entra's cold-start budget exceeds SAS's under a token-acquisition
	// profile, but equals it under the zero profile (TokenAcquire == 0).
	entraDefault := b.Cell(map[string]string{"auth": mock.AuthEntra, "delay": "default"}).(*mock.MockBackend)
	sasDefault := b.Cell(map[string]string{"auth": mock.AuthSAS, "delay": "default"}).(*mock.MockBackend)
	if entraDefault.ColdStartLatencyThreshold() <= sasDefault.ColdStartLatencyThreshold() {
		t.Errorf("entra cold-start budget %v should exceed sas %v under default profile",
			entraDefault.ColdStartLatencyThreshold(), sasDefault.ColdStartLatencyThreshold())
	}
	entraZero := b.Cell(map[string]string{"auth": mock.AuthEntra, "delay": "zero"}).(*mock.MockBackend)
	sasZero := b.Cell(map[string]string{"auth": mock.AuthSAS, "delay": "zero"}).(*mock.MockBackend)
	if entraZero.ColdStartLatencyThreshold() != sasZero.ColdStartLatencyThreshold() {
		t.Errorf("entra %v and sas %v cold-start budgets should match under zero profile",
			entraZero.ColdStartLatencyThreshold(), sasZero.ColdStartLatencyThreshold())
	}
}

// timeOneRoundTrip brings up a 1-listener, 1-sender port-forward
// topology against an in-process echo target, performs one TCP
// dial + one echo round-trip, and returns the wall time from the
// pre-dial timestamp to the post-read timestamp.
//
// Drives Setup + one Dial directly rather than reusing a Core
// scenario so the elapsed time isolates a single accept-side
// rendezvous round-trip.
func timeOneRoundTrip(t *testing.T, b scenarios.Backend) time.Duration {
	t.Helper()
	echo := scenarios.StartPlainEcho(t)
	tun := b.Setup(t, scenarios.SetupOptions{
		NumListeners:   1,
		NumSenders:     1,
		SenderMode:     scenarios.ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{echo.Addr()},
	})

	start := time.Now()
	conn, err := net.DialTimeout("tcp", tun.SenderAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial sender bind: %v", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup

	payload := []byte("ping")
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	elapsed := time.Since(start)
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: got %q want %q", got, payload)
	}
	return elapsed
}
