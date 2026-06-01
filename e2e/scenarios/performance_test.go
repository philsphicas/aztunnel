package scenarios

import (
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
