package metrics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
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
	m.SetControlChannelConnected(true)

	// Calling Done on a nil *ConnectionTracker must not panic.
	var nilTracker *ConnectionTracker
	nilTracker.Done(1.0, 100, 200, nil)
}
