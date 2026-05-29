//go:build e2e

package mock_test

import (
	"bytes"
	"io"
	"net"
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
