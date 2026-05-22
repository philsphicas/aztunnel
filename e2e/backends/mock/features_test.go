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
)

// TestMockFeature_DefaultRendezvousDelay verifies that a zero-value
// MockBackend applies DefaultRendezvousDelay (~1 s) to each accept,
// observable as a lower bound on the elapsed time for a single
// sender → listener → target round-trip.
//
// The assertion is a lower-bound only: scheduling noise can only
// make the measured elapsed time longer, never shorter, so a >=
// comparison against (DefaultRendezvousDelay - slack) is robust.
func TestMockFeature_DefaultRendezvousDelay(t *testing.T) {
	var b mock.MockBackend
	elapsed := timeOneRoundTrip(t, &b)
	wantAtLeast := mock.DefaultRendezvousDelay - 50*time.Millisecond
	if elapsed < wantAtLeast {
		t.Fatalf("round-trip took %v with default delay; want >= %v (DefaultRendezvousDelay - 50ms slack)",
			elapsed, wantAtLeast)
	}
}

// TestMockFeature_NoRendezvousDelay verifies the NoRendezvousDelay
// sentinel drops the per-accept delay back to the in-process
// baseline (~6 ms), bounded by an upper threshold well below
// DefaultRendezvousDelay so any accidental fall-through trips the
// test.
func TestMockFeature_NoRendezvousDelay(t *testing.T) {
	b := mock.MockBackend{RendezvousDelay: mock.NoRendezvousDelay}
	elapsed := timeOneRoundTrip(t, &b)
	upperBound := mock.DefaultRendezvousDelay / 2
	if elapsed > upperBound {
		t.Fatalf("round-trip took %v with NoRendezvousDelay; want < %v (DefaultRendezvousDelay/2)",
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
