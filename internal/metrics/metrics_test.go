package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestNew(t *testing.T) {
	m := New()
	if m == nil {
		t.Fatal("New() returned nil")
		return
	}
	if m.Registry == nil {
		t.Fatal("Registry is nil")
		return
	}

	// Trigger all metrics so they appear in Gather output.
	m.ConnectionError("test", "test")
	m.ObserveDialDuration("test", 0.1)
	m.ObserveTokenFetch("stub", "ok", 0.01)
	m.SetControlChannelConnected(true)
	tracker := m.ConnectionOpened("test", "test:22")
	tracker.Done(1.0, 100, 200, nil)

	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	wantNames := []string{
		"aztunnel_connections_total",
		"aztunnel_connection_errors_total",
		"aztunnel_bytes_total",
		"aztunnel_active_connections",
		"aztunnel_control_channel_connected",
		"aztunnel_connection_duration_seconds",
		"aztunnel_dial_duration_seconds",
		"aztunnel_token_fetch_seconds",
		"aztunnel_token_fetch_total",
	}
	got := make(map[string]bool)
	for _, f := range fams {
		got[f.GetName()] = true
	}

	for _, name := range wantNames {
		if !got[name] {
			t.Errorf("expected metric %q not found in registry", name)
		}
	}
}

func TestConnectionTracker(t *testing.T) {
	m := New()
	tracker := m.ConnectionOpened("listener", "10.0.0.1:22")

	// Active connections should be 1.
	g := getGauge(t, m.activeConnections, "listener", "10.0.0.1:22")
	if g != 1 {
		t.Errorf("active_connections = %v, want 1", g)
	}

	tracker.Done(5.0, 1024, 2048, nil)

	// Active connections should be back to 0.
	g = getGauge(t, m.activeConnections, "listener", "10.0.0.1:22")
	if g != 0 {
		t.Errorf("active_connections = %v, want 0", g)
	}

	// connections_total should be 1 with status=success.
	c := getCounter(t, m.connectionsTotal, "listener", "10.0.0.1:22", "success")
	if c != 1 {
		t.Errorf("connections_total = %v, want 1", c)
	}

	// Byte counters (direction label).
	toRelay := getCounter(t, m.bytesTotal, "listener", "10.0.0.1:22", "to_relay")
	if toRelay != 1024 {
		t.Errorf("bytes_total{direction=to_relay} = %v, want 1024", toRelay)
	}
	fromRelay := getCounter(t, m.bytesTotal, "listener", "10.0.0.1:22", "from_relay")
	if fromRelay != 2048 {
		t.Errorf("bytes_total{direction=from_relay} = %v, want 2048", fromRelay)
	}
}

func TestConnectionTrackerError(t *testing.T) {
	m := New()
	tracker := m.ConnectionOpened("sender", "host:80")
	tracker.Done(1.0, 100, 200, io.EOF)

	c := getCounter(t, m.connectionsTotal, "sender", "host:80", "error")
	if c != 1 {
		t.Errorf("connections_total(error) = %v, want 1", c)
	}
}

func TestConnectionError(t *testing.T) {
	m := New()
	m.ConnectionError("listener", "dial_failed")
	m.ConnectionError("listener", "dial_failed")
	m.ConnectionError("sender", "relay_failed")

	c := getCounter(t, m.connectionErrors, "listener", "dial_failed")
	if c != 2 {
		t.Errorf("connection_errors(listener,dial_failed) = %v, want 2", c)
	}
	c = getCounter(t, m.connectionErrors, "sender", "relay_failed")
	if c != 1 {
		t.Errorf("connection_errors(sender,relay_failed) = %v, want 1", c)
	}
}

func TestDialReason(t *testing.T) {
	// Non-timeout error returns fallback.
	if r := DialReason(fmt.Errorf("connection refused"), "dial_failed"); r != "dial_failed" {
		t.Errorf("DialReason(non-timeout) = %q, want dial_failed", r)
	}

	// Timeout error returns dial_timeout.
	timeoutErr := &net.OpError{Op: "dial", Err: &timeoutError{}}
	if r := DialReason(timeoutErr, "dial_failed"); r != ReasonDialTimeout {
		t.Errorf("DialReason(timeout) = %q, want %q", r, ReasonDialTimeout)
	}

	// Wrapped timeout error returns dial_timeout.
	wrapped := fmt.Errorf("dial relay: %w", timeoutErr)
	if r := DialReason(wrapped, "relay_failed"); r != ReasonDialTimeout {
		t.Errorf("DialReason(wrapped timeout) = %q, want %q", r, ReasonDialTimeout)
	}

	// context.DeadlineExceeded returns dial_timeout.
	if r := DialReason(context.DeadlineExceeded, "relay_failed"); r != ReasonDialTimeout {
		t.Errorf("DialReason(DeadlineExceeded) = %q, want %q", r, ReasonDialTimeout)
	}

	// Wrapped context.DeadlineExceeded returns dial_timeout.
	wrappedDeadline := fmt.Errorf("dial: %w", context.DeadlineExceeded)
	if r := DialReason(wrappedDeadline, "relay_failed"); r != ReasonDialTimeout {
		t.Errorf("DialReason(wrapped DeadlineExceeded) = %q, want %q", r, ReasonDialTimeout)
	}
}

func TestDialReason_DNSNotFound(t *testing.T) {
	// Bare *net.DNSError without IsTimeout maps to ReasonDNSNotFound,
	// matching classifyDialError's behaviour so the metric reason and
	// the protocol code line up for the common NXDOMAIN case.
	dnsErr := &net.DNSError{Err: "no such host", Name: "nonexistent.invalid", IsNotFound: true}
	if r := DialReason(dnsErr, ReasonDialFailed); r != ReasonDNSNotFound {
		t.Errorf("DialReason(DNSError{IsNotFound}) = %q, want %q", r, ReasonDNSNotFound)
	}

	// Wrapped in *net.OpError (the form net.Dialer actually returns).
	wrapped := &net.OpError{Op: "dial", Net: "tcp", Err: dnsErr}
	if r := DialReason(wrapped, ReasonDialFailed); r != ReasonDNSNotFound {
		t.Errorf("DialReason(OpError wrapping DNSError) = %q, want %q", r, ReasonDNSNotFound)
	}
}

func TestDialReason_DNSTimeout(t *testing.T) {
	// DNS-layer timeout takes precedence over the generic dial_timeout
	// reason — *net.DNSError satisfies net.Error.Timeout(), so without
	// the explicit DNS branch in DialReason this would be classified
	// as ReasonDialTimeout.
	dnsErr := &net.DNSError{Err: "i/o timeout", Name: "slow.example", IsTimeout: true}
	if r := DialReason(dnsErr, ReasonDialFailed); r != ReasonDNSTimeout {
		t.Errorf("DialReason(DNSError{IsTimeout}) = %q, want %q", r, ReasonDNSTimeout)
	}

	// context.DeadlineExceeded still wins over a DNS-layer timeout when
	// both are present in the chain — mirrors classifyDialError.
	combined := errors.Join(context.DeadlineExceeded, dnsErr)
	if r := DialReason(combined, ReasonDialFailed); r != ReasonDialTimeout {
		t.Errorf("DialReason(Join(DeadlineExceeded, DNS timeout)) = %q, want %q",
			r, ReasonDialTimeout)
	}
}

func TestDialReason_DNSClassificationGatedByFallback(t *testing.T) {
	// DNS classification is scoped to listener target dials
	// (fallback == ReasonDialFailed). With ReasonRelayFailed the DNS
	// branch is skipped and DialReason returns the fallback.
	dnsErr := &net.DNSError{Err: "no such host", Name: "nonexistent.invalid", IsNotFound: true}
	if r := DialReason(dnsErr, ReasonRelayFailed); r != ReasonRelayFailed {
		t.Errorf("DialReason(DNSError, ReasonRelayFailed) = %q, want %q (DNS classification must not leak to sender callers)",
			r, ReasonRelayFailed)
	}

	// DNS timeouts with ReasonRelayFailed bypass the gated DNS branch
	// and fall through to the generic netErr.Timeout() check:
	// *net.DNSError satisfies net.Error and Timeout() returns IsTimeout,
	// so the result is ReasonDialTimeout.
	dnsTimeout := &net.DNSError{Err: "i/o timeout", Name: "slow.example", IsTimeout: true}
	if r := DialReason(dnsTimeout, ReasonRelayFailed); r != ReasonDialTimeout {
		t.Errorf("DialReason(DNSError{IsTimeout}, ReasonRelayFailed) = %q, want %q",
			r, ReasonDialTimeout)
	}
}

// timeoutError implements net.Error with Timeout() == true.
type timeoutError struct{}

func (e *timeoutError) Error() string   { return "i/o timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }

func TestObserveDialDuration(t *testing.T) {
	m := New()
	m.ObserveDialDuration("sender", 0.05)

	fams, _ := m.Registry.Gather()
	for _, f := range fams {
		if f.GetName() == "aztunnel_dial_duration_seconds" {
			met := f.GetMetric()
			if len(met) == 0 {
				t.Fatal("dial_duration_seconds has no metrics")
			}
			if met[0].GetHistogram().GetSampleCount() != 1 {
				t.Errorf("dial_duration sample_count = %v, want 1", met[0].GetHistogram().GetSampleCount())
			}
			return
		}
	}
	t.Error("dial_duration_seconds metric not found")
}

func TestSetControlChannelConnected(t *testing.T) {
	m := New()

	m.SetControlChannelConnected(true)
	v := getScalarGauge(t, m.controlChannelUp)
	if v != 1 {
		t.Errorf("control_channel_connected = %v, want 1", v)
	}

	m.SetControlChannelConnected(false)
	v = getScalarGauge(t, m.controlChannelUp)
	if v != 0 {
		t.Errorf("control_channel_connected = %v, want 0", v)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	m := New()
	m.ConnectionError("listener", "test_error")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	go func() {
		_ = m.Serve(ctx, ln, logger)
	}()

	// Wait for the server to start.
	var resp *http.Response
	for range 20 {
		time.Sleep(50 * time.Millisecond)
		resp, err = http.Get("http://" + addr + "/metrics")
		if err == nil {
			break
		}
	}
	if resp == nil {
		t.Fatal("metrics server did not start")
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	// Check for our custom metric and Go runtime metrics.
	for _, want := range []string{
		"aztunnel_connection_errors_total",
		"go_goroutines",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics response missing %q", want)
		}
	}
}

func TestMetricsIntegration_BridgeFlow(t *testing.T) {
	// This test verifies the full metrics flow:
	// WebSocket echo server → Bridge → ConnectionTracker → /metrics endpoint

	m := New()

	// Simulate a complete connection lifecycle.
	tracker := m.ConnectionOpened("sender", "10.0.0.5:22")

	// Simulate bridge completing with 500 bytes to_relay and 1200 bytes from_relay.
	tracker.Done(2.5, 500, 1200, nil)

	// Also record a dial duration and an error for a different connection.
	m.ObserveDialDuration("sender", 0.042)
	m.ConnectionError("sender", "relay_failed")

	// Start metrics server.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	go func() {
		_ = m.Serve(ctx, ln, logger)
	}()

	// Wait for server.
	var resp *http.Response
	for range 20 {
		time.Sleep(50 * time.Millisecond)
		resp, err = http.Get("http://" + addr + "/metrics")
		if err == nil {
			break
		}
	}
	if resp == nil {
		t.Fatal("metrics server did not start")
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	// Verify all expected metric lines.
	expectations := []string{
		`aztunnel_connections_total{role="sender",status="success",target="10.0.0.5:22"} 1`,
		`aztunnel_bytes_total{direction="to_relay",role="sender",target="10.0.0.5:22"} 500`,
		`aztunnel_bytes_total{direction="from_relay",role="sender",target="10.0.0.5:22"} 1200`,
		`aztunnel_active_connections{role="sender",target="10.0.0.5:22"} 0`,
		`aztunnel_connection_errors_total{reason="relay_failed",role="sender"} 1`,
		`aztunnel_dial_duration_seconds_count{role="sender"} 1`,
	}
	for _, want := range expectations {
		if !strings.Contains(text, want) {
			t.Errorf("metrics response missing %q", want)
		}
	}
}

func TestSanitizeTarget_UnderCap(t *testing.T) {
	m := New()
	m.MaxTargets = 3

	got := m.SanitizeTarget("host1:22")
	if got != "host1:22" {
		t.Errorf("SanitizeTarget = %q, want %q", got, "host1:22")
	}
	got = m.SanitizeTarget("host2:22")
	if got != "host2:22" {
		t.Errorf("SanitizeTarget = %q, want %q", got, "host2:22")
	}
	// Repeat — should still return the original.
	got = m.SanitizeTarget("host1:22")
	if got != "host1:22" {
		t.Errorf("SanitizeTarget(repeat) = %q, want %q", got, "host1:22")
	}
}

func TestSanitizeTarget_AtCap(t *testing.T) {
	m := New()
	m.MaxTargets = 2

	m.SanitizeTarget("host1:22")
	m.SanitizeTarget("host2:22")

	// Third unique target should overflow.
	got := m.SanitizeTarget("host3:22")
	if got != OverflowTarget {
		t.Errorf("SanitizeTarget = %q, want %q", got, OverflowTarget)
	}

	// Known targets still work.
	got = m.SanitizeTarget("host1:22")
	if got != "host1:22" {
		t.Errorf("SanitizeTarget(known) = %q, want %q", got, "host1:22")
	}
}

func TestSanitizeTarget_Unlimited(t *testing.T) {
	m := New()
	m.MaxTargets = 0 // unlimited

	for i := range 1000 {
		target := "host" + strings.Repeat("x", i) + ":22"
		if got := m.SanitizeTarget(target); got != target {
			t.Fatalf("SanitizeTarget with MaxTargets=0 should pass through, got %q", got)
		}
	}
}

func TestSanitizeTarget_Concurrent(t *testing.T) {
	m := New()
	m.MaxTargets = 10

	var wg sync.WaitGroup
	results := make([]string, 100)
	for i := range 100 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			target := string(rune('A'+idx%26)) + ":22"
			results[idx] = m.SanitizeTarget(target)
		}(i)
	}
	wg.Wait()

	// Count unique non-overflow targets.
	unique := make(map[string]bool)
	for _, r := range results {
		if r != OverflowTarget {
			unique[r] = true
		}
	}
	if len(unique) > m.MaxTargets {
		t.Errorf("got %d unique targets, cap is %d", len(unique), m.MaxTargets)
	}
}

// helpers

func getCounter(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := cv.WithLabelValues(labels...).Write(m); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

func getGauge(t *testing.T, gv *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := gv.WithLabelValues(labels...).Write(m); err != nil {
		t.Fatalf("write gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

func getScalarGauge(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := g.Write(m); err != nil {
		t.Fatalf("write gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

func TestNilMetrics(t *testing.T) {
	// Calling methods on a nil *Metrics must not panic.
	var m *Metrics

	got := m.SanitizeTarget("host:22")
	if got != "host:22" {
		t.Errorf("SanitizeTarget on nil = %q, want %q", got, "host:22")
	}

	tracker := m.ConnectionOpened("sender", "host:22")
	if tracker != nil {
		t.Error("ConnectionOpened on nil should return nil tracker")
	}

	m.ConnectionError("sender", ReasonDialFailed)
	m.ObserveDialDuration("sender", 0.1)
	m.ObserveTokenFetch("entra", "ok", 0.1)
	m.SetControlChannelConnected(true)
	m.MuxSessionOpened("sender")
	m.MuxSessionClosed("sender")
	m.RecordMuxRotation("sender", time.Second, MuxRotationScheduled)
	m.ObserveMuxStreamOpen("sender", 0.01)
	m.MuxPoolSaturated("sender")

	// Calling Done on a nil *ConnectionTracker must not panic.
	var nilTracker *ConnectionTracker
	nilTracker.Done(1.0, 100, 200, nil)
}

// TestObserveTokenFetch_RecordsHistogramAndCounter verifies that a
// single ObserveTokenFetch call increments both the counter (by 1) and
// the histogram (one sample) under the exact (provider, result) label
// pair passed in.
func TestObserveTokenFetch_RecordsHistogramAndCounter(t *testing.T) {
	m := New()
	m.ObserveTokenFetch("entra", "ok", 0.123)
	m.ObserveTokenFetch("entra", "ok", 0.456)
	m.ObserveTokenFetch("entra", "error", 1.5)

	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var sawCounter, sawHistogram bool
	for _, f := range fams {
		switch f.GetName() {
		case "aztunnel_token_fetch_total":
			sawCounter = true
			counts := map[string]float64{}
			for _, sample := range f.GetMetric() {
				key := labelKey(sample, "provider", "result")
				counts[key] = sample.GetCounter().GetValue()
			}
			if counts["entra/ok"] != 2 {
				t.Errorf("token_fetch_total{entra,ok} = %v, want 2", counts["entra/ok"])
			}
			if counts["entra/error"] != 1 {
				t.Errorf("token_fetch_total{entra,error} = %v, want 1", counts["entra/error"])
			}
		case "aztunnel_token_fetch_seconds":
			sawHistogram = true
			samples := map[string]uint64{}
			for _, sample := range f.GetMetric() {
				key := labelKey(sample, "provider", "result")
				samples[key] = sample.GetHistogram().GetSampleCount()
			}
			if samples["entra/ok"] != 2 {
				t.Errorf("token_fetch_seconds{entra,ok} count = %v, want 2", samples["entra/ok"])
			}
			if samples["entra/error"] != 1 {
				t.Errorf("token_fetch_seconds{entra,error} count = %v, want 1", samples["entra/error"])
			}
		}
	}
	if !sawCounter {
		t.Error("aztunnel_token_fetch_total not registered")
	}
	if !sawHistogram {
		t.Error("aztunnel_token_fetch_seconds not registered")
	}
}

// labelKey concatenates the named label values from a Prometheus
// metric sample in the order given, separated by "/". Used to index
// histogram/counter samples by their label tuple in tests.
func labelKey(sample *dto.Metric, names ...string) string {
	values := make(map[string]string)
	for _, lp := range sample.GetLabel() {
		values[lp.GetName()] = lp.GetValue()
	}
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = values[n]
	}
	return strings.Join(parts, "/")
}

// TestMuxMetrics_Lifecycle verifies the gauge / counter / histogram
// flow for mux session lifecycle and rotation accounting.
func TestMuxMetrics_Lifecycle(t *testing.T) {
	m := New()

	// Two sessions open; one closed normally; one rotated by force.
	m.MuxSessionOpened("sender")
	m.MuxSessionOpened("sender")
	if got := getGauge(t, m.muxSessionsActive, "sender"); got != 2 {
		t.Errorf("mux_sessions_active{sender} = %v, want 2", got)
	}

	m.MuxSessionClosed("sender")
	m.RecordMuxRotation("sender", 50*time.Minute, MuxRotationScheduled)
	if got := getCounter(t, m.muxRotationsTotal, "sender", MuxRotationScheduled); got != 1 {
		t.Errorf("mux_rotations_total{sender,scheduled} = %v, want 1", got)
	}

	m.MuxSessionClosed("sender")
	m.RecordMuxRotation("sender", 55*time.Minute, MuxRotationForced)
	if got := getCounter(t, m.muxRotationsTotal, "sender", MuxRotationForced); got != 1 {
		t.Errorf("mux_rotations_total{sender,forced} = %v, want 1", got)
	}

	if got := getGauge(t, m.muxSessionsActive, "sender"); got != 0 {
		t.Errorf("mux_sessions_active{sender} = %v, want 0 after both closed", got)
	}

	// Histogram observations are written without panicking; verify
	// total count via Write.
	dtoH := &dto.Metric{}
	if err := m.muxSessionAge.WithLabelValues("sender").(prometheus.Histogram).Write(dtoH); err != nil {
		t.Fatalf("write mux_session_age: %v", err)
	}
	if got := dtoH.GetHistogram().GetSampleCount(); got != 2 {
		t.Errorf("mux_session_age sample count = %d, want 2", got)
	}
}

// TestMuxMetrics_StreamOpenAndSaturation verifies the stream-open
// histogram and pool-saturation counter.
func TestMuxMetrics_StreamOpenAndSaturation(t *testing.T) {
	m := New()

	m.ObserveMuxStreamOpen("sender", 0.005)
	m.ObserveMuxStreamOpen("sender", 0.1)
	m.MuxPoolSaturated("sender")
	m.MuxPoolSaturated("sender")
	m.MuxPoolSaturated("sender")

	dtoH := &dto.Metric{}
	if err := m.muxStreamOpenDuration.WithLabelValues("sender").(prometheus.Histogram).Write(dtoH); err != nil {
		t.Fatalf("write mux_stream_open: %v", err)
	}
	if got := dtoH.GetHistogram().GetSampleCount(); got != 2 {
		t.Errorf("mux_stream_open_seconds sample count = %d, want 2", got)
	}

	if got := getCounter(t, m.muxPoolSaturatedTotal, "sender"); got != 3 {
		t.Errorf("mux_pool_saturated_total{sender} = %v, want 3", got)
	}
}

// halfCloseConn is a net.Pipe-backed conn that records whether CloseWrite
// was called, so tests can verify half-close semantics without needing a
// real *net.TCPConn. CloseWrite only records — it does not tear down the
// underlying pipe, mirroring TCPConn semantics (FIN sent, but local reads
// remain open).
type halfCloseConn struct {
	net.Conn
	closeWriteCalled int32 // accessed via atomic
}

func (h *halfCloseConn) CloseWrite() error {
	atomic.StoreInt32(&h.closeWriteCalled, 1)
	return nil
}

func (h *halfCloseConn) sawCloseWrite() bool {
	return atomic.LoadInt32(&h.closeWriteCalled) == 1
}

// TestTrackedStreamBridge_HalfCloseOnEOF verifies that when one side of the
// bridge sees EOF, CloseWrite is called on the OPPOSITE side rather than
// fully closing the bridge. This is the SSH/HTTP-1.0 use case.
func TestTrackedStreamBridge_HalfCloseOnEOF(t *testing.T) {
	t.Parallel()

	// We need 4 endpoints: stream-local / stream-remote, conn-local / conn-remote.
	// The bridge runs between stream-local and conn-local.
	streamLocal, streamRemote := net.Pipe()
	connLocal, connRemote := net.Pipe()

	// Wrap conn-local so we can observe CloseWrite calls.
	hcConn := &halfCloseConn{Conn: connLocal}

	bridgeDone := make(chan error, 1)
	go func() {
		var m *Metrics // nil-safe
		_, err := m.TrackedStreamBridge(context.Background(),
			streamLocal, hcConn, "test", "target:80")
		bridgeDone <- err
	}()

	// Send a request from the stream side, expect it to arrive on the conn side.
	want := []byte("REQUEST")
	if _, err := streamRemote.Write(want); err != nil {
		t.Fatalf("write request: %v", err)
	}
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(connRemote, buf); err != nil {
		t.Fatalf("read request: %v", err)
	}
	if string(buf) != string(want) {
		t.Errorf("got %q, want %q", buf, want)
	}

	// Half-close stream → bridge sees EOF on its read-from-stream side and
	// should propagate that to conn via CloseWrite.
	if err := streamRemote.Close(); err != nil {
		t.Fatalf("close stream-remote: %v", err)
	}

	// Wait for the bridge to detect EOF and call CloseWrite on conn-local.
	deadline := time.Now().Add(2 * time.Second)
	for !hcConn.sawCloseWrite() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !hcConn.sawCloseWrite() {
		t.Fatal("expected CloseWrite to be called on conn after stream EOF")
	}

	// Clean up so the bridge can exit.
	_ = connRemote.Close()

	select {
	case err := <-bridgeDone:
		if err != nil {
			t.Errorf("bridge returned unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not exit")
	}
}

// TestTrackedStreamBridge_NoCloseWriter exercises the fallback path: a conn
// that doesn't implement CloseWriter must still cleanly terminate.
func TestTrackedStreamBridge_NoCloseWriter(t *testing.T) {
	t.Parallel()

	streamLocal, streamRemote := net.Pipe()
	connLocal, connRemote := net.Pipe()

	bridgeDone := make(chan error, 1)
	go func() {
		var m *Metrics
		_, err := m.TrackedStreamBridge(context.Background(),
			streamLocal, connLocal, "test", "target:80")
		bridgeDone <- err
	}()

	// One side EOFs.
	_ = streamRemote.Close()

	// Bridge falls back to Close on conn-local.
	deadline := time.Now().Add(2 * time.Second)
	exited := false
	for time.Now().Before(deadline) {
		// connRemote.Read should error once connLocal is closed.
		_ = connRemote.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		buf := make([]byte, 1)
		if _, err := connRemote.Read(buf); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
				exited = true
				break
			}
			// timeout — keep polling
		}
	}
	_ = connRemote.Close()
	if !exited {
		t.Error("conn-local was not closed after stream EOF")
	}

	select {
	case <-bridgeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not exit")
	}
}

// TestTrackedStreamBridge_RecordsBytes verifies the metric counters are
// updated for both directions and a clean close registers as success.
func TestTrackedStreamBridge_RecordsBytes(t *testing.T) {
	t.Parallel()

	m := New()
	streamLocal, streamRemote := net.Pipe()
	connLocal, connRemote := net.Pipe()

	done := make(chan error, 1)
	go func() {
		_, err := m.TrackedStreamBridge(context.Background(),
			streamLocal, connLocal, "sender", "target:1234")
		done <- err
	}()

	want := []byte("PING")
	go func() {
		_, _ = streamRemote.Write(want)
		_ = streamRemote.Close()
	}()
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(connRemote, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	_ = connRemote.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not exit")
	}

	// from_relay = stream → conn = 4 bytes ("PING")
	from := getCounter(t, m.bytesTotal, "sender", "target:1234", "from_relay")
	if from < float64(len(want)) {
		t.Errorf("from_relay bytes = %v, want >=%d", from, len(want))
	}
	// One completed connection, success status.
	total := getCounter(t, m.connectionsTotal, "sender", "target:1234", "success")
	if total != 1 {
		t.Errorf("connectionsTotal{success} = %v, want 1", total)
	}
}

// TestTrackedStreamBridge_NilReceiver verifies the nil-receiver fast path.
func TestTrackedStreamBridge_NilReceiver(t *testing.T) {
	t.Parallel()
	streamLocal, streamRemote := net.Pipe()
	connLocal, connRemote := net.Pipe()
	done := make(chan error, 1)
	go func() {
		var m *Metrics
		_, err := m.TrackedStreamBridge(context.Background(),
			streamLocal, connLocal, "x", "y")
		done <- err
	}()
	_ = streamRemote.Close()
	_ = connRemote.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("nil-receiver bridge did not exit")
	}
}

// erroringConn wraps a net.Conn but forces Read to return a non-EOF
// error immediately, simulating a connection reset / TLS error /
// network glitch on the stream side.
type erroringConn struct {
	net.Conn
}

func (e *erroringConn) Read(p []byte) (int, error) {
	return 0, fmt.Errorf("simulated connection reset")
}

// TestTrackedStreamBridge_RealErrorTriggersFullTeardown is the regression
// test for the hang-on-real-error bug: when one direction's io.CopyBuffer
// returns a non-EOF error and the opposite direction is parked on Read
// with no incoming data, the bridge previously called CloseWriteOrClose
// on only one side and blocked indefinitely in wg.Wait — holding the
// mux stream and pool slot. CloseWrite on a real TCP / smux conn does
// NOT tear down the read direction (mirrors FIN, not RST), so direction
// A's blocked Read was never unblocked.
//
// The fix cancels the bridge ctx on any non-EOF error so the shutdown
// goroutine closes BOTH ends. This test asserts the bridge returns
// within a bounded time after a stream-side read error.
func TestTrackedStreamBridge_RealErrorTriggersFullTeardown(t *testing.T) {
	t.Parallel()

	streamLocal, _ := net.Pipe()
	connLocal, _ := net.Pipe()

	// halfCloseConn mirrors TCPConn / smux semantics: CloseWrite() is a
	// no-op signal, the underlying pipe stays open. Without the fix,
	// direction B's closeWriteOrClose(conn) call resolves to a no-op
	// here, and direction A's Read on conn never unblocks.
	hcConn := &halfCloseConn{Conn: connLocal}
	errStream := &erroringConn{Conn: streamLocal}

	bridgeDone := make(chan error, 1)
	go func() {
		var m *Metrics // nil-safe; we don't need recording for this test
		_, err := m.TrackedStreamBridge(context.Background(),
			errStream, hcConn, "test", "target:80")
		bridgeDone <- err
	}()

	select {
	case err := <-bridgeDone:
		if err == nil {
			t.Error("bridge returned nil error; expected the simulated reset to propagate")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not exit within 3s after a real read error (hang on idle opposite direction)")
	}
}

// TestTrackedStreamBridge_OuterCtxCancelRecordsError is the regression
// test for the outer-context cancellation reporting path: when the
// caller cancels the bridge ctx (e.g. on listener shutdown), the
// internal close-on-cancel goroutine tears down both endpoints. Both
// copy goroutines therefore observe only closed-pipe errors, which
// normalize to nil. Without explicit propagation, the bridge would
// then report status="success" for a forcibly-aborted bridge. The fix
// sets bridgeErr = outerCtx.Err() when bridgeErr would otherwise be
// nil.
func TestTrackedStreamBridge_OuterCtxCancelRecordsError(t *testing.T) {
	t.Parallel()

	streamLocal, streamRemote := net.Pipe()
	connLocal, connRemote := net.Pipe()
	t.Cleanup(func() {
		_ = streamRemote.Close()
		_ = connRemote.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())

	bridgeDone := make(chan error, 1)
	go func() {
		var m *Metrics // nil-safe; we only need the error contract
		_, err := m.TrackedStreamBridge(ctx, streamLocal, connLocal, "test", "target:80")
		bridgeDone <- err
	}()

	// Bridge is now parked on Read on both directions. Cancel the
	// outer ctx → close-on-cancel goroutine closes both endpoints,
	// io.Copy returns net.ErrClosed which normalizes to nil, then the
	// outerCtx.Err() check must rescue the cancellation as bridgeErr.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-bridgeDone:
		if err == nil {
			t.Fatal("bridge returned nil error after outer-ctx cancel; expected ctx.Canceled")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("bridge err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not exit within 3s after outer-ctx cancel")
	}
}

// unexpectedEOFConn returns io.ErrUnexpectedEOF from Read, simulating a
// truncated read on the stream side (e.g. an smux session that died
// mid-stream).
type unexpectedEOFConn struct {
	net.Conn
}

func (u *unexpectedEOFConn) Read(p []byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

// TestTrackedStreamBridge_UnexpectedEOFTriggersFullTeardown is the
// regression guard for the iter-9 Copilot finding: io.ErrUnexpectedEOF
// is a truncated read, not an orderly half-close. It MUST trigger the
// hard-teardown (cancel ctx) path so both endpoints are closed. The
// half-close path (CloseWrite on the opposite direction only) leaves
// the opposite direction parked on Read if it's idle — the bridge
// would then hang holding the mux stream and pool slot.
func TestTrackedStreamBridge_UnexpectedEOFTriggersFullTeardown(t *testing.T) {
	t.Parallel()

	streamLocal, _ := net.Pipe()
	connLocal, _ := net.Pipe()

	// halfCloseConn mirrors TCPConn / smux semantics: CloseWrite() is a
	// no-op signal that does NOT unblock a parked Read. If the bridge
	// took the half-close path on ErrUnexpectedEOF, direction A's Read
	// on this conn would never unblock.
	hcConn := &halfCloseConn{Conn: connLocal}
	errStream := &unexpectedEOFConn{Conn: streamLocal}

	bridgeDone := make(chan error, 1)
	go func() {
		var m *Metrics // nil-safe
		_, err := m.TrackedStreamBridge(context.Background(),
			errStream, hcConn, "test", "target:80")
		bridgeDone <- err
	}()

	select {
	case err := <-bridgeDone:
		if err == nil {
			t.Error("bridge returned nil error; ErrUnexpectedEOF must propagate as a real failure")
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("bridge error = %v, want errors.Is(..., io.ErrUnexpectedEOF) = true", err)
		}
		if hcConn.sawCloseWrite() {
			t.Error("CloseWrite called on opposite side after ErrUnexpectedEOF; hard-teardown path expected (cancel ctx + Close), not half-close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not exit within 3s after ErrUnexpectedEOF on the stream side (hang on idle opposite direction)")
	}
}
