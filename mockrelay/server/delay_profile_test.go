package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fastProfile is a tiny but still-non-zero DelayProfile so the unit
// tests stay snappy while still distinguishing "delay-modelled" from
// "delay not modelled". Each one-way leg is 5 ms.
var fastProfile = DelayProfile{
	SLatency:          5 * time.Millisecond,
	LLatency:          5 * time.Millisecond,
	DNSLookup:         5 * time.Millisecond,
	AuthInternal:      5 * time.Millisecond,
	MatchMakeInternal: 5 * time.Millisecond,
}

// delayProfileTestServer builds a test server with the given profile,
// SkipAuth=true (so validateSAS is a no-op timing-wise — tests measure
// DelayProfile sleeps, not real SAS validation), and a discard logger.
func delayProfileTestServer(t *testing.T, p DelayProfile, opts ...Option) *httptest.Server {
	t.Helper()
	allOpts := append([]Option{WithDelayProfile(p)}, opts...)
	s, err := NewServerForTesting(Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipAuth:          true,
		RendezvousTimeout: 30 * time.Second,
	}, allOpts...)
	if err != nil {
		t.Fatalf("NewServerForTesting: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// wsURLOf converts the httptest.Server URL prefix to ws://.
func wsURLOf(srvURL string) string {
	return strings.Replace(srvURL, "http://", "ws://", 1)
}

// dialConnect dials the sender connect endpoint.
func dialConnect(ctx context.Context, srvURL, entity string) (*websocket.Conn, *http.Response, error) {
	u := wsURLOf(srvURL) + "/$hc/" + entity + "?sb-hc-action=connect&sb-hc-token=irrelevant"
	return websocket.Dial(ctx, u, nil)
}

// acceptEnvelope is the minimal shape of the JSON frame the relay
// writes to the listener control WS when a sender arrives.
type acceptEnvelope struct {
	Accept struct {
		Address string `json:"address"`
		ID      string `json:"id"`
	} `json:"accept"`
}

// withinTolerance asserts that observed is within [lower, upper].
func withinTolerance(t *testing.T, label string, observed, lower, upper time.Duration) {
	t.Helper()
	if observed < lower || observed > upper {
		t.Errorf("%s: observed %v outside [%v, %v]", label, observed, lower, upper)
	}
}

// TestDelayProfile_ZeroIsFast asserts the Go zero-value DelayProfile
// applies no synthetic delay anywhere — listener dial completes
// promptly.
func TestDelayProfile_ZeroIsFast(t *testing.T) {
	srv := delayProfileTestServer(t, DelayProfile{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, closeWS := dialListener(t, ctx, srv.URL, "zero-fast")
	defer closeWS()
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("zero-profile listener dial took %v, want < 500ms", elapsed)
	}
}

// TestDelayProfile_HandleListenWallClock asserts handleListen pays
// DNS + (handshake+wsGet+response)*L + AuthInternal before emitting
// the 101 response, measured from the client's perspective.
func TestDelayProfile_HandleListenWallClock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping wall-clock DelayProfile test in -short mode")
	}
	srv := delayProfileTestServer(t, fastProfile)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Predicted: DNS + (handshake+wsGet+response)*L + AuthInternal
	//          = 5  + (4+1+1)*5                     + 5
	//          = 40ms
	predicted := fastProfile.DNSLookup +
		(hopsHandshake+hopsWSGet+hopsResponse)*fastProfile.LLatency +
		fastProfile.AuthInternal

	start := time.Now()
	_, closeWS := dialListener(t, ctx, srv.URL, "listen-wallclock")
	defer closeWS()
	elapsed := time.Since(start)

	withinTolerance(t, "handleListen wall-clock", elapsed, predicted, predicted+500*time.Millisecond)
}

// TestDelayProfile_HandleConnectWallClock asserts handleConnect pays
// the sender-side rendezvous setup. The sender's 101 is gated by
// handleAccept's takePending (which itself requires the listener's
// accept-dial entry transit), so we assert a lower bound of:
// 2*DNS + (handshake+wsGet+response)*S +
// (handshake+wsGet+response)*L + AuthInternal + MatchMakeInternal.
func TestDelayProfile_HandleConnectWallClock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping wall-clock DelayProfile test in -short mode")
	}
	srv := delayProfileTestServer(t, fastProfile)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Stand up a listener that reads the accept frame and dials the
	// rendezvous URL (so the bridge actually pairs).
	lws, closeListener := dialListener(t, ctx, srv.URL, "connect-wallclock")
	defer closeListener()
	go runAcceptEchoer(ctx, lws)

	predicted := 2*fastProfile.DNSLookup +
		(hopsHandshake+hopsWSGet+hopsResponse)*fastProfile.SLatency +
		(hopsHandshake+hopsWSGet+hopsResponse)*fastProfile.LLatency +
		fastProfile.AuthInternal + fastProfile.MatchMakeInternal

	start := time.Now()
	sws, _, err := dialConnect(ctx, srv.URL, "connect-wallclock")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("dialConnect: %v", err)
	}
	defer func() { _ = sws.Close(websocket.StatusNormalClosure, "test done") }()

	withinTolerance(t, "handleConnect wall-clock", elapsed, predicted, predicted+500*time.Millisecond)
}

// TestDelayProfile_BridgeRoundTripPaysOneRTT asserts that a single
// request-reply through the bridge pays exactly one round-trip of
// pipelined wire propagation: 2 * (S + L). With S = L = 50 ms the
// predicted wall-clock is 200 ms; we assert [120 ms, 600 ms].
func TestDelayProfile_BridgeRoundTripPaysOneRTT(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping wall-clock DelayProfile test in -short mode")
	}
	p := DelayProfile{
		SLatency: 50 * time.Millisecond,
		LLatency: 50 * time.Millisecond,
	}
	srv := delayProfileTestServer(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lws, closeListener := dialListener(t, ctx, srv.URL, "bridge-rtt")
	defer closeListener()
	go runAcceptEchoer(ctx, lws)

	sws, _, err := dialConnect(ctx, srv.URL, "bridge-rtt")
	if err != nil {
		t.Fatalf("dialConnect: %v", err)
	}
	defer func() { _ = sws.Close(websocket.StatusNormalClosure, "test done") }()

	start := time.Now()
	if err := sws.Write(ctx, websocket.MessageBinary, []byte("ping")); err != nil {
		t.Fatalf("sender write: %v", err)
	}
	if _, _, err := sws.Read(ctx); err != nil {
		t.Fatalf("sender read: %v", err)
	}
	elapsed := time.Since(start)

	// Predicted 2*(S+L) = 200 ms. Lower bound is 1.2*RTT to ride out
	// scheduling jitter without admitting a "no delay at all" bug;
	// upper bound is 3*RTT for CI noise.
	lower := 120 * time.Millisecond
	upper := 600 * time.Millisecond
	withinTolerance(t, "bridge single echo", elapsed, lower, upper)
}

// TestDelayProfile_BridgePipelinesStream asserts the bridge pipelines
// concurrently in-flight messages instead of serialising them. With
// S = L = 20 ms, 50 back-to-back echoes (writer keeps writing while
// reader is draining) should complete in roughly one one-way fill of
// the pipe plus one round trip, on the order of low hundreds of ms —
// NOT 50 * 2*(S+L) = 4 s as a stop-and-wait bridge would cost.
func TestDelayProfile_BridgePipelinesStream(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping wall-clock DelayProfile test in -short mode")
	}
	p := DelayProfile{
		SLatency: 20 * time.Millisecond,
		LLatency: 20 * time.Millisecond,
	}
	srv := delayProfileTestServer(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lws, closeListener := dialListener(t, ctx, srv.URL, "bridge-pipeline")
	defer closeListener()
	go runAcceptEchoer(ctx, lws)

	sws, _, err := dialConnect(ctx, srv.URL, "bridge-pipeline")
	if err != nil {
		t.Fatalf("dialConnect: %v", err)
	}
	defer func() { _ = sws.Close(websocket.StatusNormalClosure, "test done") }()

	const messages = 50

	// Drain reads concurrently with writes so neither direction
	// blocks the other — this is what a real pipelined stream looks
	// like (e.g. SOCKS5 bulk transfer).
	readErr := make(chan error, 1)
	go func() {
		for i := 0; i < messages; i++ {
			if _, _, err := sws.Read(ctx); err != nil {
				readErr <- err
				return
			}
		}
		readErr <- nil
	}()

	start := time.Now()
	for i := 0; i < messages; i++ {
		if err := sws.Write(ctx, websocket.MessageBinary, []byte("x")); err != nil {
			t.Fatalf("sender write %d: %v", i, err)
		}
	}
	if err := <-readErr; err != nil {
		t.Fatalf("sender read: %v", err)
	}
	elapsed := time.Since(start)

	// Lower bound: at least one one-way propagation must have
	// happened — anything below that means the bridge isn't actually
	// pipelining the synthetic delay. Use 0.5*(S+L) to absorb
	// scheduling jitter while still rejecting the "no delay" bug.
	lower := 20 * time.Millisecond
	// Upper bound: pipelined cost is roughly 2*(S+L) plus a small
	// per-message overhead. Stop-and-wait would be 50 * 2*(S+L) =
	// 4 s, so anything under ~1 s definitively proves we pipelined.
	upper := 1 * time.Second
	withinTolerance(t, "bridge pipelined stream", elapsed, lower, upper)
}

// TestDelayProfile_BridgeSequentialEchoesAreSerial asserts the
// flip-side of pipelining: when each echo blocks waiting for its
// reply before sending the next, the bridge correctly pays one round
// trip per echo, in contrast to the back-to-back stream test above
// which fills the pipe and amortises the propagation cost.
func TestDelayProfile_BridgeSequentialEchoesAreSerial(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping wall-clock DelayProfile test in -short mode")
	}
	p := DelayProfile{
		SLatency: 20 * time.Millisecond,
		LLatency: 20 * time.Millisecond,
	}
	srv := delayProfileTestServer(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lws, closeListener := dialListener(t, ctx, srv.URL, "bridge-serial")
	defer closeListener()
	go runAcceptEchoer(ctx, lws)

	sws, _, err := dialConnect(ctx, srv.URL, "bridge-serial")
	if err != nil {
		t.Fatalf("dialConnect: %v", err)
	}
	defer func() { _ = sws.Close(websocket.StatusNormalClosure, "test done") }()

	const echoes = 10
	start := time.Now()
	for i := 0; i < echoes; i++ {
		if err := sws.Write(ctx, websocket.MessageBinary, []byte("x")); err != nil {
			t.Fatalf("sender write %d: %v", i, err)
		}
		if _, _, err := sws.Read(ctx); err != nil {
			t.Fatalf("sender read %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	// Predicted echoes * 2*(S+L) = 10 * 80 ms = 800 ms.
	// Lower bound 0.6x to reject "no delay"; upper bound 3x for CI
	// noise.
	lower := 480 * time.Millisecond
	upper := 2400 * time.Millisecond
	withinTolerance(t, "bridge sequential echoes", elapsed, lower, upper)
}

// TestDelayProfile_AcceptUnknownIDPaysResponseLeg asserts that the
// handleAccept 404 path pays both the entry wire transit AND the
// response leg.
func TestDelayProfile_AcceptUnknownIDPaysResponseLeg(t *testing.T) {
	srv := delayProfileTestServer(t, fastProfile)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := wsURLOf(srv.URL) + "/$hc/no-such-entity?sb-hc-action=accept&id=ZZZZZZZZZZZZZZZZ"

	start := time.Now()
	_, _, err := websocket.Dial(ctx, wsURL, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("dial accept: expected failure for unknown id")
	}

	predicted := fastProfile.DNSLookup +
		(hopsHandshake+hopsWSGet+hopsResponse)*fastProfile.LLatency
	withinTolerance(t, "404 accept lane", elapsed, predicted, predicted+500*time.Millisecond)
}

// TestDelayProfile_ContextCancelDuringSleepPromptlyExits asserts that
// the handler does not block past the request context deadline when
// the lane sleeps are running.
func TestDelayProfile_ContextCancelDuringSleepPromptlyExits(t *testing.T) {
	p := DelayProfile{
		SLatency: 500 * time.Millisecond,
		LLatency: 500 * time.Millisecond,
	}
	srv := delayProfileTestServer(t, p)

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	dialDone := make(chan error, 1)
	go func() {
		_, _, err := websocket.Dial(ctx, wsURLOf(srv.URL)+"/$hc/cancel-mid?sb-hc-action=listen", nil)
		dialDone <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancelStart := time.Now()
	cancel()

	select {
	case <-dialDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("dial did not unblock within 2s after context cancel")
	}
	elapsed := time.Since(cancelStart)
	if elapsed > 500*time.Millisecond {
		t.Errorf("dial took %v after cancel; expected prompt exit", elapsed)
	}

	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+5 {
		t.Errorf("goroutine leak: before=%d after=%d", before, after)
	}
}

// TestDelayProfile_NoListenerReturns404Lane asserts that handleConnect
// returns 404 when no listener is registered, AND pays the full
// sender-side request + response lane transit.
func TestDelayProfile_NoListenerReturns404Lane(t *testing.T) {
	srv := delayProfileTestServer(t, fastProfile)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, resp, err := dialConnect(ctx, srv.URL, "nobody-listening")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected dial failure when no listener registered")
	}
	if resp == nil {
		t.Fatalf("expected non-nil HTTP response on 404")
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}

	predicted := fastProfile.DNSLookup +
		(hopsHandshake+hopsWSGet+hopsResponse)*fastProfile.SLatency +
		fastProfile.AuthInternal + fastProfile.MatchMakeInternal
	withinTolerance(t, "no-listener 404 lane", elapsed, predicted, predicted+500*time.Millisecond)
}

// TestDelayProfile_AuthFailurePaysLane asserts that the listener-side
// 401 path pays DNS + request + AuthInternal + response — the auth
// failure does NOT skip the response leg.
func TestDelayProfile_AuthFailurePaysLane(t *testing.T) {
	s, err := NewServerForTesting(Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipAuth:          false,
		RendezvousTimeout: 30 * time.Second,
	}, WithDelayProfile(fastProfile))
	if err != nil {
		t.Fatalf("NewServerForTesting: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := wsURLOf(srv.URL) + "/$hc/auth-fail?sb-hc-action=listen"
	start := time.Now()
	_, resp, err := websocket.Dial(ctx, wsURL, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected dial failure on missing token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got resp=%v err=%v", resp, err)
	}

	predicted := fastProfile.DNSLookup +
		(hopsHandshake+hopsWSGet+hopsResponse)*fastProfile.LLatency +
		fastProfile.AuthInternal
	withinTolerance(t, "401 auth-fail lane", elapsed, predicted, predicted+500*time.Millisecond)
}

// TestProfileRegistry_KnownNames asserts the registry resolves its
// canonical names to the expected profiles and that ProfileNames
// returns them sorted. Adding a profile to the registry should extend
// — not break — these expectations.
func TestProfileRegistry_KnownNames(t *testing.T) {
	cases := map[string]DelayProfile{
		"zero":    DelayProfileZero,
		"default": DelayProfileDefault,
	}
	for name, want := range cases {
		got, err := ProfileByName(name)
		if err != nil {
			t.Fatalf("ProfileByName(%q): unexpected error %v", name, err)
		}
		if got != want {
			t.Errorf("ProfileByName(%q) = %+v, want %+v", name, got, want)
		}
	}

	names := ProfileNames()
	if !sort.StringsAreSorted(names) {
		t.Errorf("ProfileNames() not sorted: %v", names)
	}
	for name := range cases {
		if !slices.Contains(names, name) {
			t.Errorf("ProfileNames() %v missing %q", names, name)
		}
	}
}

// TestProfileByName_UnknownIsLoud asserts an unregistered name yields
// an error naming the bad input and listing the known profiles, so a
// typo at a selection site fails loudly rather than silently picking
// the wrong timing model.
func TestProfileByName_UnknownIsLoud(t *testing.T) {
	_, err := ProfileByName("does-not-exist")
	if err == nil {
		t.Fatalf("ProfileByName(unknown): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error %q does not name the unknown profile", err.Error())
	}
	for _, name := range ProfileNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not list known profile %q", err.Error(), name)
		}
	}
}

// TestProfileRegistry_AllValid asserts every registered profile passes
// WithDelayProfile validation, so a future profile with a negative
// field is caught by this unit test rather than at e2e selection time.
func TestProfileRegistry_AllValid(t *testing.T) {
	for _, name := range ProfileNames() {
		p, err := ProfileByName(name)
		if err != nil {
			t.Fatalf("ProfileByName(%q): %v", name, err)
		}
		if err := p.validate(); err != nil {
			t.Errorf("registered profile %q fails validation: %v", name, err)
		}
	}
}

// runAcceptEchoer reads accept frames from a listener control WS,
// dials each rendezvous URL, and echoes every message. Used by the
// tests that need a real bridge to pair on the listener side.
func runAcceptEchoer(ctx context.Context, lws *websocket.Conn) {
	for {
		_, data, err := lws.Read(ctx)
		if err != nil {
			return
		}
		var env acceptEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Accept.Address == "" {
			continue
		}
		go func(addr string) {
			rws, _, err := websocket.Dial(ctx, addr, nil)
			if err != nil {
				return
			}
			defer func() { _ = rws.Close(websocket.StatusNormalClosure, "echoer done") }()
			for {
				typ, m, err := rws.Read(ctx)
				if err != nil {
					return
				}
				if err := rws.Write(ctx, typ, m); err != nil {
					return
				}
			}
		}(env.Accept.Address)
	}
}

// TestRendezvousTimeoutOn101Transit asserts that if RendezvousTimeout
// fires *after* pairing but during the 101-transit SLatency sleep,
// the sender receives a 504 pre-upgrade rather than an emitted 101
// torn down a moment later. Guards the waitCtx-on-101-sleep fix.
func TestRendezvousTimeoutOn101Transit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping wall-clock timeout test in -short mode")
	}
	// hopsResponse=1, so post-pair sleep is exactly SLatency.
	// Pre-handleConnect work uses the parent ctx (5 s) and is paid
	// in full; only the post-pair sleep is bounded by waitCtx.
	// SLatency=200 ms means a 200 ms post-pair sleep, vs.
	// RendezvousTimeout=200 ms: pairing burns ~50-100 ms of the
	// budget, leaving 100-150 ms for the 200 ms sleep, which then
	// busts through and triggers the new 504 path. LLatency stays
	// tiny so the listener's accept-side handleAccept finishes
	// pairing well inside the 200 ms budget (else the test would
	// hit the *pre*-101 504 path instead and not exercise the fix).
	p := DelayProfile{
		SLatency:          200 * time.Millisecond,
		LLatency:          1 * time.Millisecond,
		AuthInternal:      1 * time.Millisecond,
		MatchMakeInternal: 1 * time.Millisecond,
	}
	s, err := NewServerForTesting(Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipAuth:          true,
		RendezvousTimeout: 200 * time.Millisecond,
	}, WithDelayProfile(p))
	if err != nil {
		t.Fatalf("NewServerForTesting: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lws, closeListener := dialListener(t, ctx, srv.URL, "timeout-101")
	defer closeListener()
	go runAcceptEchoer(ctx, lws)

	_, resp, err := dialConnect(ctx, srv.URL, "timeout-101")
	if err == nil {
		t.Fatalf("expected dial failure on rendezvous timeout, got nil err")
	}
	if resp == nil || resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 GatewayTimeout, got resp=%v err=%v", resp, err)
	}
}

// TestWithDelayProfile_RejectsNegativeDuration ensures that
// WithDelayProfile fails fast on any negative field instead of
// silently distorting the pipelinedCopy drain budget.
func TestWithDelayProfile_RejectsNegativeDuration(t *testing.T) {
	cases := []struct {
		name string
		p    DelayProfile
	}{
		{"SLatency", DelayProfile{SLatency: -1 * time.Millisecond}},
		{"LLatency", DelayProfile{LLatency: -1 * time.Millisecond}},
		{"DNSLookup", DelayProfile{DNSLookup: -1 * time.Millisecond}},
		{"AuthInternal", DelayProfile{AuthInternal: -1 * time.Millisecond}},
		{"MatchMakeInternal", DelayProfile{MatchMakeInternal: -1 * time.Millisecond}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewServerForTesting(Config{
				Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
				SkipAuth:          true,
				RendezvousTimeout: 30 * time.Second,
			}, WithDelayProfile(c.p))
			if err == nil {
				t.Fatalf("expected error for negative %s, got nil", c.name)
			}
			if !strings.Contains(err.Error(), c.name) {
				t.Fatalf("error %q does not mention field %s", err.Error(), c.name)
			}
		})
	}
}

// TestWithDelayProfile_AcceptsZero confirms the zero value (all
// fields zero) passes validation and is the documented default.
func TestWithDelayProfile_AcceptsZero(t *testing.T) {
	_, err := NewServerForTesting(Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipAuth:          true,
		RendezvousTimeout: 30 * time.Second,
	}, WithDelayProfile(DelayProfile{}))
	if err != nil {
		t.Fatalf("zero DelayProfile rejected: %v", err)
	}
}
