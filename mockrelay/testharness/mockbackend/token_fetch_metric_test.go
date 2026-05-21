package mockbackend

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/internal/listener"
	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/philsphicas/aztunnel/internal/sender"
	"github.com/philsphicas/aztunnel/mockrelay/server"
	dto "github.com/prometheus/client_model/go"
)

// erroringTokenProvider is a TokenProvider that always returns the
// configured error. Used to drive the result=error path through the
// wrapper end-to-end against the shared Metrics surface — the dial
// machinery itself does not retry on token-fetch errors, so we
// exercise the error path by calling GetToken on the wrapped provider
// directly rather than through DialWithRetry.
type erroringTokenProvider struct{ err error }

func (e *erroringTokenProvider) GetToken(context.Context, string) (string, error) {
	return "", e.err
}

// TestTokenFetchMetric_MockRelay verifies that relay.WithMetrics,
// when applied at the production wiring point (around a real
// TokenProvider passed into listener.Config.TokenProvider /
// sender.PortForwardConfig.TokenProvider), produces observations on
// the wrapped Metrics surface:
//
//   - result=ok: a real listener + sender topology drives a successful
//     round-trip through the mock relay; the wrapper records every
//     GetToken call the listener (control-channel attach + renews) and
//     the sender (each dial) make against the underlying SAS provider.
//
//   - result=error: a separately wrapped TokenProvider, primed to
//     always error, is exercised directly. The wrapper's contract is
//     identical regardless of who calls GetToken — DialWithRetry does
//     not retry on token errors, so driving the error path through the
//     full sender machinery would short-circuit the wrapper's
//     observation budget after a single bridge-killing attempt. Direct
//     invocation keeps the assertion focused on the wrapper's
//     integration with the metrics surface, while the unit tests in
//     internal/relay cover the wrapper's own behaviour.
//
// This is a mockrelay-only integration test. The e2e Azure test
// (e2e/token_fetch_metric_test.go) covers the result=ok path against
// real Entra/SAS providers.
func TestTokenFetchMetric_MockRelay(t *testing.T) {
	host, clientOpts := startMockRelay(t, DefaultRendezvousDelay)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	entity := mustEntityName(t)
	silentLogger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Share one metrics surface between the listener and the sender so
	// the final assertion can read the merged token_fetch_total /
	// _seconds samples from a single registry. In production each
	// process has its own surface, but the wrapper is the same code
	// path regardless of how many providers point at the same
	// Metrics, so the merge here doesn't lose any property under test.
	m := metrics.New()
	sasInner := relay.TokenProvider(&relay.SASTokenProvider{
		KeyName: server.DefaultSASKeyName,
		Key:     server.DefaultSASKey,
	})
	listenerTP := relay.WithMetrics(sasInner, m, relay.ProviderSAS)
	senderTP := relay.WithMetrics(sasInner, m, relay.ProviderSAS)

	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	t.Cleanup(func() { _ = echoLn.Close() })
	go runEchoServer(echoLn)

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
			Metrics:       m,
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

	// Drive one successful round-trip. The listener's control-channel
	// attach has already produced its first ok observation; the
	// sender's dial here produces the next one.
	conn, err := net.DialTimeout("tcp", senderAddr.String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial sender: %v", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	payload := []byte("token-fetch-metric\n")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}

	// Now drive the error path through a separately wrapped provider
	// against the same shared metrics surface, exercising the wrapper
	// end-to-end (caller → wrapper → observer → registry) without
	// going through DialWithRetry, which would not retry on token
	// errors and would short-circuit the wrapper after a single
	// bridge-killing attempt.
	errSentinel := errors.New("simulated upstream failure")
	failingTP := relay.WithMetrics(&erroringTokenProvider{err: errSentinel}, m, relay.ProviderSAS)
	for i := 0; i < 2; i++ {
		if _, err := failingTP.GetToken(ctx, "ignored"); !errors.Is(err, errSentinel) {
			t.Fatalf("failingTP.GetToken: got %v, want wraps %v", err, errSentinel)
		}
	}

	// Wait until the success counter reaches at least 2 (one from the
	// listener control-channel attach, one from the sender dial). The
	// listener's renew loop may produce more between observations; we
	// only assert the floor.
	if !waitForCounter(m, "aztunnel_token_fetch_total", relay.ProviderSAS, "ok", 2, 5*time.Second) {
		t.Fatalf("token_fetch_total{provider=sas,result=ok} never reached 2 (got %v)",
			counterValue(m, "aztunnel_token_fetch_total", relay.ProviderSAS, "ok"))
	}
	if got := counterValue(m, "aztunnel_token_fetch_total", relay.ProviderSAS, "error"); got != 2 {
		t.Errorf("token_fetch_total{provider=sas,result=error} = %v, want 2", got)
	}

	// Both histograms must have observations matching their counter
	// sample counts (counter and histogram are paired in
	// ObserveTokenFetch).
	if got := histogramCount(m, "aztunnel_token_fetch_seconds", relay.ProviderSAS, "ok"); got < 2 {
		t.Errorf("token_fetch_seconds{provider=sas,result=ok} count = %v, want >= 2", got)
	}
	if got := histogramCount(m, "aztunnel_token_fetch_seconds", relay.ProviderSAS, "error"); got != 2 {
		t.Errorf("token_fetch_seconds{provider=sas,result=error} count = %v, want 2", got)
	}
}

// runEchoServer accepts connections and echoes bytes until the
// listener is closed. The error from Accept after Close is expected
// and not surfaced.
func runEchoServer(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer conn.Close() //nolint:errcheck // best-effort cleanup
			_, _ = io.Copy(conn, conn)
		}(c)
	}
}

// waitForCounter polls m's registry for the given counter to reach at
// least want under the (provider, result) label pair before timeout.
// Returns true on success, false on deadline.
func waitForCounter(m *metrics.Metrics, name, provider, result string, want float64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if counterValue(m, name, provider, result) >= want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// counterValue returns the single-sample value of a counter under the
// (provider, result) label pair from m's registry. Returns 0 if the
// counter is missing or has no matching label combination.
func counterValue(m *metrics.Metrics, name, provider, result string) float64 {
	families, err := m.Registry.Gather()
	if err != nil {
		return 0
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, sample := range f.GetMetric() {
			if matchesProviderResult(sample.GetLabel(), provider, result) {
				return sample.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// histogramCount returns the SampleCount of a histogram under the
// (provider, result) label pair from m's registry.
func histogramCount(m *metrics.Metrics, name, provider, result string) uint64 {
	families, err := m.Registry.Gather()
	if err != nil {
		return 0
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, sample := range f.GetMetric() {
			if matchesProviderResult(sample.GetLabel(), provider, result) {
				return sample.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

// matchesProviderResult reports whether a metric sample's labels match
// the given (provider, result) pair exactly. Used to look up specific
// label combinations in counter/histogram samples without depending on
// label ordering.
func matchesProviderResult(labels []*dto.LabelPair, provider, result string) bool {
	var sawProvider, sawResult bool
	for _, lp := range labels {
		switch lp.GetName() {
		case "provider":
			if lp.GetValue() != provider {
				return false
			}
			sawProvider = true
		case "result":
			if lp.GetValue() != result {
				return false
			}
			sawResult = true
		}
	}
	return sawProvider && sawResult
}
