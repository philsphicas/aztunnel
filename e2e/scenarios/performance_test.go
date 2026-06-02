package scenarios

import (
	"strings"
	"sync/atomic"
	"testing"
)

func tunnelWithLogCounters(senderCalls, listenerCalls *atomic.Int32) *Tunnel {
	return &Tunnel{
		Senders: []*Sender{
			{Logs: func() string { senderCalls.Add(1); return "sender log" }},
			nil,         // nil handle must be skipped
			{Logs: nil}, // nil accessor must be skipped
		},
		Listeners: []*Listener{
			{Logs: func() string { listenerCalls.Add(1); return "listener log" }},
			nil,
			{Logs: nil},
		},
	}
}

// TestDumpTunnelLogs verifies the dump reads exactly the non-nil
// sender/listener accessors and tolerates nil handles / nil accessors
// without panicking.
func TestDumpTunnelLogs(t *testing.T) {
	var senderCalls, listenerCalls atomic.Int32
	dumpTunnelLogs(t, tunnelWithLogCounters(&senderCalls, &listenerCalls))
	if got := senderCalls.Load(); got != 1 {
		t.Errorf("sender Logs called %d times, want 1", got)
	}
	if got := listenerCalls.Load(); got != 1 {
		t.Errorf("listener Logs called %d times, want 1", got)
	}
}

// TestFilterTraceLines keeps only the rendezvous-trace lines (both the
// relay and accept variants, including "(dial failed)") and drops
// everything else, returning "" when none match.
func TestFilterTraceLines(t *testing.T) {
	in := strings.Join([]string{
		`time=... level=DEBUG msg="dialing relay" bridge_id=A`,
		`time=... level=DEBUG msg="relay rendezvous trace" bridge_id=A resp_wait=300ms`,
		`time=... level=DEBUG msg="relay connected" bridge_id=A`,
		`time=... level=DEBUG msg="accept rendezvous trace" bridge_id=A resp_wait=6.7s`,
		`time=... level=DEBUG msg="relay rendezvous trace (dial failed)" bridge_id=B`,
		`time=... level=INFO msg="something else"`,
	}, "\n")
	got := filterTraceLines(in)
	want := strings.Join([]string{
		`time=... level=DEBUG msg="relay rendezvous trace" bridge_id=A resp_wait=300ms`,
		`time=... level=DEBUG msg="accept rendezvous trace" bridge_id=A resp_wait=6.7s`,
		`time=... level=DEBUG msg="relay rendezvous trace (dial failed)" bridge_id=B`,
	}, "\n")
	if got != want {
		t.Errorf("filterTraceLines:\n got=%q\nwant=%q", got, want)
	}
	if got := filterTraceLines("no traces here\njust noise"); got != "" {
		t.Errorf("filterTraceLines(no matches) = %q, want empty", got)
	}
}

// TestDumpTunnelRendezvousTraces verifies the lean dump reads each
// non-nil accessor exactly once and tolerates nil handles / accessors.
// (It emits only the trace lines; the filtering itself is covered by
// TestFilterTraceLines.)
func TestDumpTunnelRendezvousTraces(t *testing.T) {
	var senderCalls, listenerCalls atomic.Int32
	dumpTunnelRendezvousTraces(t, tunnelWithLogCounters(&senderCalls, &listenerCalls))
	if got := senderCalls.Load(); got != 1 {
		t.Errorf("sender Logs called %d times, want 1", got)
	}
	if got := listenerCalls.Load(); got != 1 {
		t.Errorf("listener Logs called %d times, want 1", got)
	}
}

// TestDumpConnectLatencyLogsOnFail_SilentOnSuccess verifies the
// failure-gated cleanup does not touch the captured buffers when the
// scenario passes.
func TestDumpConnectLatencyLogsOnFail_SilentOnSuccess(t *testing.T) {
	var senderCalls, listenerCalls atomic.Int32
	t.Run("inner", func(inner *testing.T) {
		dumpConnectLatencyLogsOnFail(inner, tunnelWithLogCounters(&senderCalls, &listenerCalls))
		// inner passes — cleanup must stay silent.
	})
	if got := senderCalls.Load(); got != 0 {
		t.Errorf("sender Logs called %d times on success, want 0", got)
	}
	if got := listenerCalls.Load(); got != 0 {
		t.Errorf("listener Logs called %d times on success, want 0", got)
	}
}
