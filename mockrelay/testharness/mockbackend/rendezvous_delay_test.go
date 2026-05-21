package mockbackend_test

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/internal/testharness/e2escenarios"
	"github.com/philsphicas/aztunnel/mockrelay/testharness/mockbackend"
)

// TestMockBackend_DefaultRendezvousDelay verifies that a zero-value
// MockBackend applies DefaultRendezvousDelay (~1 s) to each accept,
// observable as a lower bound on the elapsed time for a single
// sender→listener→target round-trip.
//
// The assertion is a lower-bound only: scheduling noise on a loaded
// CI runner can only make the measured elapsed time longer, never
// shorter, so a >= comparison against (DefaultRendezvousDelay - slack)
// is robust.
func TestMockBackend_DefaultRendezvousDelay(t *testing.T) {
	var b mockbackend.MockBackend
	elapsed := timeOneRoundTrip(t, &b)
	// 50 ms of slack absorbs timer-resolution differences between
	// the t.Now() observation and the server's first sleep
	// observation. Tune up if you see flakes; never down.
	wantAtLeast := mockbackend.DefaultRendezvousDelay - 50*time.Millisecond
	if elapsed < wantAtLeast {
		t.Fatalf("round-trip took %v with default delay; want >= %v (DefaultRendezvousDelay - 50ms slack)", elapsed, wantAtLeast)
	}
}

// TestMockBackend_NoRendezvousDelay verifies that the
// NoRendezvousDelay sentinel drops the per-accept delay back to
// the in-process baseline (~6 ms), bounded by an upper threshold
// well below DefaultRendezvousDelay so any accidental fall-through
// to the default trips the test.
func TestMockBackend_NoRendezvousDelay(t *testing.T) {
	b := mockbackend.MockBackend{RendezvousDelay: mockbackend.NoRendezvousDelay}
	elapsed := timeOneRoundTrip(t, &b)
	// 500 ms is exactly half of DefaultRendezvousDelay and well
	// above the in-process baseline (~6 ms + tens of ms of TCP/
	// accept/echo overhead) plus any reasonable CI scheduling noise.
	// Any value under 500 ms confirms the opt-out is live.
	upperBound := mockbackend.DefaultRendezvousDelay / 2
	if elapsed > upperBound {
		t.Fatalf("round-trip took %v with NoRendezvousDelay; want < %v (DefaultRendezvousDelay/2). Wiring of the negative-sentinel opt-out is likely broken.", elapsed, upperBound)
	}
}

// timeOneRoundTrip brings up a 1-listener, 1-sender port-forward
// topology against an in-process echo target, performs one TCP
// dial + one echo round-trip, and returns the wall time from the
// pre-dial timestamp to the post-read timestamp. All resources are
// torn down via t.Cleanup registered by Setup and StartPlainEcho.
//
// The helper drives Setup + one Dial directly rather than reusing a
// Core scenario so the elapsed time isolates a single accept-side
// rendezvous round-trip. Core scenarios open multiple connections
// and apply leak / sampler instrumentation, both of which would
// dilute the timing signal the assertions in this file rely on.
func timeOneRoundTrip(t *testing.T, b e2escenarios.Backend) time.Duration {
	t.Helper()
	echo := e2escenarios.StartPlainEcho(t)
	tun := b.Setup(t, e2escenarios.SetupOptions{
		NumListeners:   1,
		NumSenders:     1,
		SenderMode:     e2escenarios.ModePortForward,
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
