//go:build e2e

package mock

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/internal/listener"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/sender"
	"github.com/philsphicas/aztunnel/mockrelay/server"
)

// TestMockEmulates_EntraTokenAcquisitionDelay demonstrates that the
// in-process mock can reproduce the entra-vs-sas timing divergence we
// measure against real Azure Relay — a one-off cold-start token-fetch
// cost on each process's first dial, absorbed thereafter by the client
// token cache — purely from the harness side, with no production change.
//
// It asserts two things:
//
//   - Cache correctness (the hard check): across many serial dials the
//     sender acquires a token exactly once, proving EntraTokenProvider's
//     cache is consulted. Listener acquires once for its control channel.
//
//   - Cold-start shape (a coarse timing check): the first connection
//     through the freshly-started sender pays roughly the acquisition
//     delay on top of the rendezvous cost; subsequent connections do
//     not. Kept loose (> delay/2) because rendezvous DelayProfile sleeps
//     and CI scheduling add noise.
func TestMockEmulates_EntraTokenAcquisitionDelay(t *testing.T) {
	const acquireDelay = 450 * time.Millisecond
	const dials = 6

	host, clientOpts := startMockRelay(t, server.DelayProfileDefault)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })

	entity := mustEntityName(t)
	silentLogger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Separate providers => separate caches => each "process" pays its
	// own cold acquisition, exactly as separate aztunnel processes do.
	listenerTP, listenerCred := newFakeEntraProvider(acquireDelay)
	senderTP, senderCred := newFakeEntraProvider(acquireDelay)

	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	t.Cleanup(func() { _ = echoLn.Close() })
	go runEchoServerEmul(echoLn)

	m := metrics.New()
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := listener.ListenAndServe(ctx, listener.Config{
			Endpoint:      host,
			EntityPath:    entity,
			TokenProvider: listenerTP,
			ClientOptions: clientOpts,
			AllowList:     []string{echoLn.Addr().String()},
			Logger:        silentLogger,
			Metrics:       m,
		})
		if err != nil && ctx.Err() == nil {
			t.Logf("listener exited: %v", err)
		}
	}()
	if !waitForGauge(m, "aztunnel_control_channel_connected", 1, 15*time.Second) {
		t.Fatalf("listener never reported control_channel_connected")
	}

	addrCh := make(chan net.Addr, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := sender.PortForward(ctx, sender.PortForwardConfig{
			Endpoint:      host,
			EntityPath:    entity,
			TokenProvider: senderTP,
			ClientOptions: clientOpts,
			Target:        echoLn.Addr().String(),
			BindAddress:   "127.0.0.1:0",
			Logger:        silentLogger,
			Metrics:       metrics.New(),
			Ready: func(a net.Addr) {
				select {
				case addrCh <- a:
				default:
				}
			},
		})
		if err != nil && ctx.Err() == nil {
			t.Logf("sender exited: %v", err)
		}
	}()

	var senderAddr net.Addr
	select {
	case senderAddr = <-addrCh:
	case <-time.After(15 * time.Second):
		t.Fatalf("sender never became ready")
	}

	durations := make([]time.Duration, dials)
	for i := 0; i < dials; i++ {
		durations[i] = echoRoundTrip(t, senderAddr.String())
	}

	// Hard check: the cache was consulted. One acquisition for the
	// sender across all dials; one for the listener's control channel.
	if got := senderCred.calls.Load(); got != 1 {
		t.Errorf("sender token acquisitions = %d, want 1 (cache not consulted across %d dials)", got, dials)
	}
	if got := listenerCred.calls.Load(); got != 1 {
		t.Errorf("listener token acquisitions = %d, want 1", got)
	}

	// Coarse cold-start shape: first dial pays ~acquireDelay more than
	// the steady-state median. Loose threshold to tolerate rendezvous
	// sleeps + CI noise.
	var rest time.Duration
	for _, d := range durations[1:] {
		rest += d
	}
	median := rest / time.Duration(dials-1)
	coldPremium := durations[0] - median
	t.Logf("cold dial=%s steady-median=%s cold-premium=%s (modelled acquireDelay=%s)",
		durations[0], median, coldPremium, acquireDelay)
	if coldPremium < acquireDelay/2 {
		t.Errorf("cold-start premium = %s, want > %s (cold acquisition cost not visible)",
			coldPremium, acquireDelay/2)
	}
}

// echoRoundTrip dials the sender's local bind, writes one line, reads
// the echo back, and returns the wall-clock time for the round trip.
func echoRoundTrip(t *testing.T, addr string) time.Duration {
	t.Helper()
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Fatalf("dial sender: %v", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	payload := []byte("entra-cold-start\n")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	return time.Since(start)
}
