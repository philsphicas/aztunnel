package sender

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/philsphicas/aztunnel/internal/metrics"
	"github.com/philsphicas/aztunnel/internal/muxconfig"
	"github.com/philsphicas/aztunnel/internal/protocol"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/xtaci/smux"
)

// TestSendEnvelopeOverStream_NoBannerOverread is the critical regression
// test for the bug the rubber-duck review caught: using json.NewDecoder
// on an smux stream lets the decoder's internal buffer swallow target
// bytes that arrive immediately after the response.
//
// Many real-world target protocols send bytes immediately on connect —
// server-first banners (SSH "SSH-2.0…", SMTP "220…") or client-first
// startup payloads (Postgres, HTTP). In either case the listener writes
// ConnectResponse{OK:true} and then immediately proxies the target
// socket; the first payload bytes follow the response with no gap. The
// sender MUST be able to see those bytes.
func TestSendEnvelopeOverStream_NoBannerOverread(t *testing.T) {
	clientConn, serverConn := net.Pipe()

	clientCfg := smuxClientCfg()
	serverCfg := smuxClientCfg() // same config both sides for keepalive parity

	clientSess, err := smux.Client(clientConn, clientCfg)
	if err != nil {
		t.Fatalf("smux.Client: %v", err)
	}
	defer clientSess.Close() //nolint:errcheck // test cleanup
	serverSess, err := smux.Server(serverConn, serverCfg)
	if err != nil {
		t.Fatalf("smux.Server: %v", err)
	}
	defer serverSess.Close() //nolint:errcheck // test cleanup

	const banner = "SSH-2.0-OpenSSH_9.0\r\n"

	serverErr := make(chan error, 1)
	go func() {
		stream, err := serverSess.AcceptStream()
		if err != nil {
			serverErr <- err
			return
		}
		defer stream.Close() //nolint:errcheck // test cleanup

		env, err := protocol.ReadStreamEnvelope(stream)
		if err != nil {
			serverErr <- err
			return
		}
		if env.Target != "ssh-host:22" {
			serverErr <- errors.New("unexpected target: " + env.Target)
			return
		}
		if err := protocol.WriteStreamResponse(stream, protocol.ConnectResponse{
			Version: protocol.CurrentVersion,
			OK:      true,
		}); err != nil {
			serverErr <- err
			return
		}
		// Immediately push the "target" banner with no gap.
		if _, err := stream.Write([]byte(banner)); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	stream, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	if _, err := sendEnvelopeOverStream(context.Background(), stream, "ssh-host:22", "", 0); err != nil {
		t.Fatalf("sendEnvelopeOverStream: %v", err)
	}

	// Read what's left on the stream after sendEnvelopeOverStream
	// returns. With length-prefixed framing this is exactly the banner.
	// With json.NewDecoder it would be empty (banner consumed into the
	// decoder's buffer and discarded), so this assertion catches the
	// regression.
	got := make([]byte, len(banner))
	if err := stream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatalf("read banner: %v (got %q)", err, got)
	}
	if string(got) != banner {
		t.Fatalf("banner = %q, want %q", got, banner)
	}

	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("server: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not finish in time")
	}
}

// TestSendEnvelopeOverStream_Rejection covers the listener-rejects-target
// case (allowlist denial, dial failure). The error should bubble up with
// the listener's message intact.
func TestSendEnvelopeOverStream_Rejection(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	clientSess, err := smux.Client(clientConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Client: %v", err)
	}
	defer clientSess.Close() //nolint:errcheck // test cleanup
	serverSess, err := smux.Server(serverConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Server: %v", err)
	}
	defer serverSess.Close() //nolint:errcheck // test cleanup

	go func() {
		stream, err := serverSess.AcceptStream()
		if err != nil {
			return
		}
		defer stream.Close() //nolint:errcheck // test cleanup
		if _, err := protocol.ReadStreamEnvelope(stream); err != nil {
			return
		}
		_ = protocol.WriteStreamResponse(stream, protocol.ConnectResponse{
			Version: protocol.CurrentVersion,
			Error:   "target not allowed",
		})
	}()

	stream, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	_, err = sendEnvelopeOverStream(context.Background(), stream, "blocked-host:22", "", 0)
	if err == nil {
		t.Fatal("expected error for rejected target")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("target not allowed")) {
		t.Errorf("error = %v, want it to contain 'target not allowed'", err)
	}
}

// TestSendEnvelopeOverStream_StreamDeath covers the case where the smux
// session dies after the envelope is sent but before a response arrives
// (e.g. WS dropped). sendEnvelopeOverStream must return an error rather
// than block forever.
func TestSendEnvelopeOverStream_StreamDeath(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	clientSess, err := smux.Client(clientConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Client: %v", err)
	}
	defer clientSess.Close() //nolint:errcheck // test cleanup
	serverSess, err := smux.Server(serverConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Server: %v", err)
	}

	// Server: accept a stream, read the envelope, then tear down the
	// session before responding.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		stream, err := serverSess.AcceptStream()
		if err != nil {
			return
		}
		_, _ = protocol.ReadStreamEnvelope(stream)
		_ = serverSess.Close()
	}()

	stream, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup
	_ = stream.SetReadDeadline(time.Now().Add(2 * time.Second))

	_, err = sendEnvelopeOverStream(context.Background(), stream, "host:22", "", 0)
	if err == nil {
		t.Fatal("expected error after session death, got nil")
	}
	wg.Wait()
}

// TestSendEnvelopeOverStream_DeadlineUnresponsivePeer is the regression
// guard for the "envelope read has no deadline, so a peer that accepts
// the smux stream but never replies pins the pool slot until process
// shutdown" finding. The server here accepts the stream and reads the
// envelope but never sends a response. sendEnvelopeOverStream must
// return within muxStreamHandshakeTimeout (capped here at 1s via
// ctx.Deadline so the test is fast).
func TestSendEnvelopeOverStream_DeadlineUnresponsivePeer(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	clientSess, err := smux.Client(clientConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Client: %v", err)
	}
	defer clientSess.Close() //nolint:errcheck // test cleanup
	serverSess, err := smux.Server(serverConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Server: %v", err)
	}
	defer serverSess.Close() //nolint:errcheck // test cleanup

	// Server: accept the stream, read the envelope, then sit forever
	// without sending a response.
	var wg sync.WaitGroup
	wg.Add(1)
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		stream, err := serverSess.AcceptStream()
		if err != nil {
			return
		}
		defer stream.Close() //nolint:errcheck // test cleanup
		_, _ = protocol.ReadStreamEnvelope(stream)
		<-stop // never reply
	}()

	stream, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	// Caller ctx with a 1s deadline — sendEnvelopeOverStream should
	// honour the tighter of (ctx deadline, muxStreamHandshakeTimeout).
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := sendEnvelopeOverStream(ctx, stream, "host:22", "", 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from unresponsive peer, got nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sendEnvelopeOverStream blocked > 3s on unresponsive peer (deadline not applied)")
	}
	close(stop)
	wg.Wait()
}

// TestSendEnvelopeOverStream_CtxCancelUnresponsivePeer asserts that a
// caller-driven ctx cancellation (e.g. user kills curl) unblocks the
// envelope exchange even when neither the stream deadline nor any
// other timeout has fired.
func TestSendEnvelopeOverStream_CtxCancelUnresponsivePeer(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	clientSess, err := smux.Client(clientConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Client: %v", err)
	}
	defer clientSess.Close() //nolint:errcheck // test cleanup
	serverSess, err := smux.Server(serverConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Server: %v", err)
	}
	defer serverSess.Close() //nolint:errcheck // test cleanup

	var wg sync.WaitGroup
	wg.Add(1)
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		stream, err := serverSess.AcceptStream()
		if err != nil {
			return
		}
		defer stream.Close() //nolint:errcheck // test cleanup
		_, _ = protocol.ReadStreamEnvelope(stream)
		<-stop // never reply
	}()

	stream, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := sendEnvelopeOverStream(ctx, stream, "host:22", "", 0)
		done <- err
	}()

	// Give the goroutine a moment to enter ReadStreamResponse.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after ctx cancel, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sendEnvelopeOverStream did not return within 2s of ctx cancel")
	}
	close(stop)
	wg.Wait()
}

func TestIsMuxUnsupportedRejection(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"missing target", true},
		{"Missing target", true},
		{"unsupported protocol version", true},
		{"Unsupported Protocol Version 99", true},
		{"invalid envelope", true},
		{"target not allowed", false},
		{"connection failed", false},
		{"connection refused", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			if got := isMuxUnsupportedRejection(tt.msg); got != tt.want {
				t.Errorf("isMuxUnsupportedRejection(%q) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}

func TestNewMuxDialer_Defaults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := NewMuxDialer(ctx, "https://example.servicebus.windows.net", "test-hc", stubTokenProvider{}, relay.ClientOptions{}, nil, nil)
	if d == nil {
		t.Fatal("NewMuxDialer returned nil")
	}
	if d.logger == nil {
		t.Error("logger should default to slog.Default()")
	}
	if d.parentCtx != ctx {
		t.Error("parentCtx not preserved")
	}
	// Close should be safe on an un-connected dialer.
	d.Close()
	d.Close() // idempotent
}

func TestMuxDialer_StickyFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tp := stubTokenProvider{}
	d := NewMuxDialer(ctx, "https://example.servicebus.windows.net", "test-hc", tp, relay.ClientOptions{}, nil, nil)
	// Simulate a prior rejection — the rejection is sticky for
	// muxUnavailableTTL so concurrent callers fast-path to v1 without
	// re-paying the rendezvous cost.
	d.muxUnavailUntil.Store(time.Now().Add(muxUnavailableTTL).UnixNano())

	_, err := d.OpenStream(ctx)
	if !errors.Is(err, ErrMuxUnsupported) {
		t.Fatalf("OpenStream should return ErrMuxUnsupported during sticky window, got %v", err)
	}

	// After the TTL elapses, the next call attempts a real dial. We use
	// a pre-cancelled ctx to force a fast deterministic failure that is
	// NOT ErrMuxUnsupported.
	d.muxUnavailUntil.Store(time.Now().Add(-time.Second).UnixNano())
	cancelledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	_, err = d.OpenStream(cancelledCtx)
	if errors.Is(err, ErrMuxUnsupported) {
		t.Fatal("after TTL expiry, OpenStream should attempt a real dial, not return ErrMuxUnsupported")
	}
	if err == nil {
		t.Fatal("expected dial error against cancelled ctx")
	}
}

// TestMuxDialer_TerminalAfterCtxCancel guards the "no zombie reconnect"
// invariant that MuxPool relies on for safe per-session eviction during
// graceful rotation. Once the dialer's parentCtx is cancelled (the pool
// cancels each session's per-session ctx in evictSession), OpenStream
// must NOT attempt a fresh dial — it must return ErrMuxDialerClosed.
//
// Without this guarantee, a goroutine that reserved a slot on session
// S, then raced past the pool's selector before S was evicted, could
// trigger an untracked relay rendezvous on an "already-evicted" dialer.
func TestMuxDialer_TerminalAfterCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := NewMuxDialer(ctx, "https://example.servicebus.windows.net", "test-hc", stubTokenProvider{}, relay.ClientOptions{}, nil, nil)
	cancel()

	_, err := d.OpenStream(context.Background())
	if !errors.Is(err, ErrMuxDialerClosed) {
		t.Fatalf("expected ErrMuxDialerClosed after parentCtx cancel, got %v", err)
	}
}

// TestMuxDialer_RemoteClosedSession_NoGaugeLeak is the regression test
// for the mux_sessions_active leak that occurred when a session was
// closed by the peer (or hit smux keepalive death) and OpenStream was
// then called again on the same dialer. The old code path took the
// `d.session != nil` branch but the OpenStream call returned an error,
// triggering reconnect through connectLocked without first running
// closeSessionLocked — leaking one gauge increment per dead session.
//
// The fix in OpenStream calls closeSessionLocked() when IsClosed() is
// true, so a subsequent connectLocked starts from a clean slate.
func TestMuxDialer_RemoteClosedSession_NoGaugeLeak(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := metrics.New()

	d := NewMuxDialer(
		ctx,
		"https://example.servicebus.windows.net",
		"test-hc",
		stubTokenProvider{},
		relay.ClientOptions{},
		nil,
		m,
	)

	// Build a real smux session paired across net.Pipe, then close it
	// so IsClosed() returns true. The dialer treats this as a remotely
	// closed session.
	clientConn, serverConn := net.Pipe()
	clientSess, err := smux.Client(clientConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Client: %v", err)
	}
	serverSess, err := smux.Server(serverConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Server: %v", err)
	}
	t.Cleanup(func() { _ = serverSess.Close() })

	// Simulate a prior successful connect by incrementing the gauge,
	// then assigning the session and closing it.
	m.MuxSessionOpened("sender")
	d.session = clientSess
	if err := clientSess.Close(); err != nil {
		t.Fatalf("close clientSess: %v", err)
	}
	if !clientSess.IsClosed() {
		t.Fatal("clientSess should be closed")
	}

	// OpenStream goes through the IsClosed() branch, calls
	// closeSessionLocked (gauge → 0), then connectLocked. We use a
	// pre-cancelled ctx so the dial fails immediately rather than
	// attempting a real DNS lookup against example.servicebus.windows.net.
	// The gauge decrement happens before connectLocked, so an early
	// dial failure does not affect the assertion.
	cancelledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	_, _ = d.OpenStream(cancelledCtx)

	if got := gaugeValue(t, m.Registry, "aztunnel_mux_sessions_active", "sender"); got != 0 {
		t.Fatalf("mux_sessions_active{sender} = %v after OpenStream on a closed session, want 0 (gauge leak)", got)
	}
}

// TestMuxDialer_FastPathHonorsCallerCtx is the regression test for the
// per-call ctx hole on the established-session fast path. The smux
// session's OpenStream is not ctx-aware, so without an explicit
// re-check the dialer would return a stream past the caller's
// admission deadline. The fix closes the just-opened stream and
// returns ctx.Err() so the pool's ctx-typed-error handling path can
// release the slot and surface the cancellation to the caller.
func TestMuxDialer_FastPathHonorsCallerCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := metrics.New()
	d := NewMuxDialer(
		ctx,
		"https://example.servicebus.windows.net",
		"test-hc",
		stubTokenProvider{},
		relay.ClientOptions{},
		nil,
		m,
	)

	// Plant a real, open smux session in the dialer so the
	// established-session fast path is reachable without a relay
	// dial. Mirrors the setup in
	// TestMuxDialer_RemoteClosedSession_NoGaugeLeak.
	clientConn, serverConn := net.Pipe()
	clientSess, err := smux.Client(clientConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Client: %v", err)
	}
	serverSess, err := smux.Server(serverConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Server: %v", err)
	}
	t.Cleanup(func() {
		_ = clientSess.Close()
		_ = serverSess.Close()
	})
	// Accept-loop on the server side so the fast-path
	// clientSess.OpenStream completes (smux requires the peer to
	// accept the new stream).
	go func() {
		for {
			s, err := serverSess.AcceptStream()
			if err != nil {
				return
			}
			_ = s.Close()
		}
	}()
	d.session = clientSess

	// Pre-cancelled caller ctx. The dialer's parentCtx is still
	// alive, so without the fix the fast path would happily return
	// a stream.
	cancelledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	stream, err := d.OpenStream(cancelledCtx)
	if err == nil {
		_ = stream.Close()
		t.Fatalf("OpenStream returned a stream for pre-cancelled ctx; want ctx.Canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("OpenStream err = %v, want context.Canceled", err)
	}
}

// gaugeValue reads a single label combination of a gauge from a
// Prometheus registry. Returns 0 if the metric / label combination is
// absent.
func gaugeValue(t *testing.T, reg interface {
	Gather() ([]*dto.MetricFamily, error)
}, name, role string,
) float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, fam := range fams {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "role" && lp.GetValue() == role {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	return 0
}

// counterValue reads a counter from a Prometheus registry, summing
// across all samples whose labels match the supplied label/value pairs.
// Pairs are flat: ["role", "sender", "reason", "relay_failed"].
// Returns 0 if no matching samples are found.
func counterValue(t *testing.T, reg interface {
	Gather() ([]*dto.MetricFamily, error)
}, name string, pairs ...string,
) float64 {
	t.Helper()
	if len(pairs)%2 != 0 {
		t.Fatalf("counterValue: pairs length must be even, got %d", len(pairs))
	}
	want := make(map[string]string, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		want[pairs[i]] = pairs[i+1]
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var total float64
	for _, fam := range fams {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			match := true
			for k, v := range want {
				if got[k] != v {
					match = false
					break
				}
			}
			if match {
				total += m.GetCounter().GetValue()
			}
		}
	}
	return total
}

// stubTokenProvider is a minimal TokenProvider for tests that need a
// non-nil tp but never actually dial (ctx is cancelled first).
type stubTokenProvider struct{}

func (stubTokenProvider) GetToken(ctx context.Context, resourceURI string) (string, error) {
	return "", ctx.Err()
}

// smuxClientCfg returns the same config muxdialer.go uses, so tests use
// the production timings (specifically the keepalive interval that must be
// shorter than Azure Relay's idle timeout in production).
func smuxClientCfg() *smux.Config {
	return muxconfig.SmuxConfig()
}

// TestMergeCancelCtx_CancelsOnA verifies the merged ctx fires when the
// caller's ctx (a) is cancelled — preserving normal "caller gave up"
// semantics.
func TestMergeCancelCtx_CancelsOnA(t *testing.T) {
	t.Parallel()
	a, cancelA := context.WithCancel(context.Background())
	b, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	merged, cancelMerged := mergeCancelCtx(a, b)
	defer cancelMerged()

	cancelA()
	select {
	case <-merged.Done():
	case <-time.After(time.Second):
		t.Fatal("merged ctx did not fire when a was cancelled")
	}
}

// TestMergeCancelCtx_CancelsOnB is the regression test for the Fix A
// concurrency hole: when the pool evicts a dialer, it cancels the
// dialer's parentCtx (b). Without mergeCancelCtx, a slow in-flight dial
// scoped only to the caller's ctx would NOT see the eviction signal and
// could publish an untracked session after eviction completed.
//
// The merged ctx MUST fire when b alone is cancelled.
func TestMergeCancelCtx_CancelsOnB(t *testing.T) {
	t.Parallel()
	a, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	b, cancelB := context.WithCancel(context.Background())

	merged, cancelMerged := mergeCancelCtx(a, b)
	defer cancelMerged()

	cancelB()
	select {
	case <-merged.Done():
	case <-time.After(time.Second):
		t.Fatal("merged ctx did not fire when b (parentCtx) was cancelled — eviction would not abort dial")
	}
}

// TestMergeCancelCtx_ReleasesWatcher ensures the watcher goroutine
// exits when neither ctx fires and the caller invokes the returned
// cancel func — i.e. the helper does not leak goroutines on the happy
// path where the dial completes before any ctx is cancelled.
func TestMergeCancelCtx_ReleasesWatcher(t *testing.T) {
	t.Parallel()
	a, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	b, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	merged, cancelMerged := mergeCancelCtx(a, b)
	cancelMerged() // simulates the deferred cancel after a successful dial

	select {
	case <-merged.Done():
	case <-time.After(time.Second):
		t.Fatal("merged ctx did not fire after cancelMerged was called")
	}
	// Goroutine leak would be detected by -race / -count=1 / leaktest
	// frameworks. The select{stop} branch should fire.
}

// TestMergeCancelCtx_CancelIdempotent verifies the returned cancel func
// is safe to call multiple times. The internal `close(stop)` would
// panic on a second call without sync.Once protection — a sharp edge
// that would land if a future refactor added an explicit dialCancel()
// alongside the defer (common pattern).
func TestMergeCancelCtx_CancelIdempotent(t *testing.T) {
	t.Parallel()
	a, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	b, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	_, cancelMerged := mergeCancelCtx(a, b)
	cancelMerged()
	cancelMerged() // must NOT panic
}

// TestConnectLocked_RewritesErrorOnParentCancel is the regression test
// for the eviction-during-dial bug GPT-5.5 flagged. When d.parentCtx
// is cancelled while connectLocked is in flight, the underlying dial
// returns a wrapped context.Canceled error like "dial relay: ... :
// context canceled". MuxPool.OpenStream only retries on
// ErrMuxDialerClosed — so without the defer rewrite, the caller would
// see a hard failure instead of the pool selecting another session.
//
// This test pre-cancels parentCtx so the underlying relay.DialWithRetry
// fails immediately (real DNS skipped), then asserts the returned error
// is ErrMuxDialerClosed via errors.Is.
func TestConnectLocked_RewritesErrorOnParentCancel(t *testing.T) {
	t.Parallel()

	parent, cancelParent := context.WithCancel(context.Background())
	d := NewMuxDialer(parent, "https://example.servicebus.windows.net", "test-hc",
		stubTokenProvider{}, relay.ClientOptions{}, nil, nil)
	cancelParent()

	d.mu.Lock()
	err := d.connectLocked(context.Background())
	d.mu.Unlock()

	if !errors.Is(err, ErrMuxDialerClosed) {
		t.Fatalf("expected ErrMuxDialerClosed when parentCtx is cancelled mid-dial, got %v", err)
	}
}

// TestWatchSession_DecrementsGaugeOnAsyncClose is the regression test
// for the gauge-leak Copilot flagged: when a smux session is closed
// asynchronously (peer close, keepalive timeout, transport error),
// nothing was observing that state until the next OpenStream call —
// so aztunnel_mux_sessions_active could report a dead session as
// active for arbitrarily long.
//
// The fix spawns a watcher goroutine that blocks on sess.AcceptStream
// (NOT sess.CloseChan — smux v1.5.57 only fires CloseChan on explicit
// Close, not on transport-level errors). When AcceptStream returns an
// error, the watcher decrements via closeSessionLocked once the
// session dies. This test publishes a live session into d.session,
// increments the gauge to simulate the post-connect state, starts the
// watcher, then breaks the transport from the peer side and asserts
// the gauge drops to 0 within a bounded time without any intervening
// OpenStream call.
func TestWatchSession_DecrementsGaugeOnAsyncClose(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := metrics.New()
	d := NewMuxDialer(
		ctx,
		"https://example.servicebus.windows.net",
		"test-hc",
		stubTokenProvider{},
		relay.ClientOptions{},
		nil,
		m,
	)

	clientConn, serverConn := net.Pipe()
	clientSess, err := smux.Client(clientConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Client: %v", err)
	}
	serverSess, err := smux.Server(serverConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Server: %v", err)
	}
	t.Cleanup(func() { _ = serverSess.Close() })

	// Simulate the post-connectLocked state.
	m.MuxSessionOpened("sender")
	d.session = clientSess
	if got := gaugeValue(t, m.Registry, "aztunnel_mux_sessions_active", "sender"); got != 1 {
		t.Fatalf("setup: mux_sessions_active{sender} = %v, want 1", got)
	}

	go d.watchSession(clientSess)

	// Trigger async close (simulates peer-side close / keepalive
	// timeout / network error): closing the underlying pipe from the
	// server side breaks the transport. smux's recvLoop hits a read
	// error and AcceptStream returns — which is what the watcher is
	// blocked on.
	_ = serverConn.Close()

	// Poll the gauge until it drops to 0 (bounded by smux keepalive
	// detection + watcher scheduling). 2 seconds is generous.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gaugeValue(t, m.Registry, "aztunnel_mux_sessions_active", "sender") == 0 {
			return // success
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("mux_sessions_active{sender} = %v after async session close, want 0 (watcher did not fire / decrement leaked)",
		gaugeValue(t, m.Registry, "aztunnel_mux_sessions_active", "sender"))
}

// TestWatchSession_NoDoubleDecrementWithSyncClose verifies that the
// watcher does NOT double-decrement when closeSessionLocked is called
// synchronously from another path (e.g. MuxDialer.Close or the
// IsClosed() branch of OpenStream). The watcher's `d.session == sess`
// guard must skip the second decrement.
func TestWatchSession_NoDoubleDecrementWithSyncClose(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := metrics.New()
	d := NewMuxDialer(
		ctx,
		"https://example.servicebus.windows.net",
		"test-hc",
		stubTokenProvider{},
		relay.ClientOptions{},
		nil,
		m,
	)

	clientConn, serverConn := net.Pipe()
	clientSess, err := smux.Client(clientConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Client: %v", err)
	}
	serverSess, err := smux.Server(serverConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Server: %v", err)
	}
	t.Cleanup(func() {
		_ = serverSess.Close()
		_ = serverConn.Close()
	})

	m.MuxSessionOpened("sender")
	d.session = clientSess

	go d.watchSession(clientSess)

	// Synchronous close path: simulates MuxDialer.Close racing with the
	// watcher. closeSessionLocked decrements first, sets d.session=nil;
	// the watcher then sees d.session != sess and must skip.
	d.Close()

	// Wait for the watcher to observe the close + drop the lock.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		// Watcher will eventually return from AcceptStream, acquire
		// d.mu, see d.session != sess (because the sync Close path
		// already nilled it) and exit without decrementing again.
		// Repeated reads of the gauge stay at 0.
		if gaugeValue(t, m.Registry, "aztunnel_mux_sessions_active", "sender") != 0 {
			t.Fatalf("gauge went negative or above 0 — double-decrement: %v",
				gaugeValue(t, m.Registry, "aztunnel_mux_sessions_active", "sender"))
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := gaugeValue(t, m.Registry, "aztunnel_mux_sessions_active", "sender"); got != 0 {
		t.Fatalf("final gauge = %v, want 0", got)
	}
}

// TestSendEnvelopeOverStream_RespectsCustomTimeout verifies that a
// caller-supplied handshakeTimeout (smaller than the package default)
// bounds the envelope+response exchange. Regression guard for the
// configurable --mux-stream-handshake-timeout flag.
func TestSendEnvelopeOverStream_RespectsCustomTimeout(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	clientSess, err := smux.Client(clientConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Client: %v", err)
	}
	defer clientSess.Close() //nolint:errcheck // test cleanup
	serverSess, err := smux.Server(serverConn, smuxClientCfg())
	if err != nil {
		t.Fatalf("smux.Server: %v", err)
	}
	defer serverSess.Close() //nolint:errcheck // test cleanup

	var wg sync.WaitGroup
	wg.Add(1)
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		stream, err := serverSess.AcceptStream()
		if err != nil {
			return
		}
		defer stream.Close() //nolint:errcheck // test cleanup
		_, _ = protocol.ReadStreamEnvelope(stream)
		<-stop // never reply
	}()

	stream, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	// Custom 300ms timeout. Default is 60s; if the custom value is not
	// honoured this test would block until that fires.
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := sendEnvelopeOverStream(context.Background(), stream, "host:22", "", 300*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected timeout error, got nil")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("custom handshakeTimeout not honoured: returned after %v, expected ~300ms", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sendEnvelopeOverStream blocked > 2s with custom 300ms handshakeTimeout")
	}
	close(stop)
	wg.Wait()
}

// TestMuxDialer_UnavailableLockFree is the regression guard for the
// finding that opener.Unavailable() called under p.mu could block the
// entire pool when the dialer mutex (d.mu) was held by a long relay
// dial. Unavailable() must be a lock-free atomic read.
func TestMuxDialer_UnavailableLockFree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := NewMuxDialer(ctx, "https://example.servicebus.windows.net", "test-hc",
		stubTokenProvider{}, relay.ClientOptions{}, nil, nil)

	// Acquire d.mu in a separate goroutine and hold it for 500ms,
	// simulating an in-flight relay dial under connectLocked.
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		close(held)
		<-release
	}()
	<-held
	defer close(release)

	// Unavailable() must return quickly even though d.mu is held.
	type result struct {
		val     bool
		elapsed time.Duration
	}
	res := make(chan result, 1)
	go func() {
		t0 := time.Now()
		v := d.Unavailable()
		res <- result{val: v, elapsed: time.Since(t0)}
	}()
	select {
	case r := <-res:
		if r.elapsed > 50*time.Millisecond {
			t.Errorf("Unavailable() took %v while d.mu was held; must be lock-free", r.elapsed)
		}
		if r.val {
			t.Errorf("unset dialer should report Unavailable()=false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Unavailable() blocked on d.mu — regression in lock-free read")
	}

	// Repeat with the sticky flag armed to confirm the positive case
	// also stays lock-free.
	d.muxUnavailUntil.Store(time.Now().Add(muxUnavailableTTL).UnixNano())
	go func() {
		t0 := time.Now()
		v := d.Unavailable()
		res <- result{val: v, elapsed: time.Since(t0)}
	}()
	select {
	case r := <-res:
		if r.elapsed > 50*time.Millisecond {
			t.Errorf("Unavailable() took %v while d.mu was held (armed); must be lock-free", r.elapsed)
		}
		if !r.val {
			t.Errorf("armed dialer should report Unavailable()=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Unavailable() blocked on d.mu while armed")
	}
}

// TestMuxDialer_ParentCancelSuppressesDialMetric is the regression
// guard for the false-positive `connection_errors_total{relay_failed}`
// emission when parentCtx is cancelled mid-dial (internal pool
// eviction/rotation/shutdown). The dial returns ctx.Canceled but
// because parentCtx is also done, the failure is an internal signal
// the pool will recover from — not a relay-side failure.
func TestMuxDialer_ParentCancelSuppressesDialMetric(t *testing.T) {
	m := metrics.New()
	parentCtx, cancel := context.WithCancel(context.Background())
	d := NewMuxDialer(parentCtx, "https://test.invalid", "test-hc",
		stubTokenProvider{}, relay.ClientOptions{}, nil, m)

	// Cancel parentCtx BEFORE the dial — DialWithRetry will return
	// ctx.Canceled immediately without network I/O, and the
	// parentCtx-suppression path must not call ConnectionError.
	cancel()

	// Bypass OpenStream's fast-path parentCtx check so we exercise
	// connectLocked's dial+metric handling directly.
	d.mu.Lock()
	err := d.connectLocked(context.Background())
	d.mu.Unlock()

	if !errors.Is(err, ErrMuxDialerClosed) {
		t.Fatalf("expected ErrMuxDialerClosed, got %v", err)
	}

	// Sum across all reasons — the false metric appears as
	// reason=relay_failed today, but the suppression must hold
	// regardless of how DialReason classifies a context.Canceled.
	for _, reason := range []string{
		metrics.ReasonRelayFailed,
		metrics.ReasonDialTimeout,
		metrics.ReasonDialFailed,
	} {
		if got := counterValue(t, m.Registry, "aztunnel_connection_errors_total", "role", "sender", "reason", reason); got != 0 {
			t.Errorf("connection_errors_total{sender, %s} = %v, want 0 (parent-cancel must not record dial failure)", reason, got)
		}
	}
}
