package scenarios

import (
	"os"
	"os/exec"
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

// TestDumpTunnelLogsOnFail_SilentOnSuccess verifies the
// failure-gated cleanup does not touch the captured buffers when the
// scenario passes.
func TestDumpTunnelLogsOnFail_SilentOnSuccess(t *testing.T) {
	var senderCalls, listenerCalls atomic.Int32
	t.Run("inner", func(inner *testing.T) {
		dumpTunnelLogsOnFail(inner, tunnelWithLogCounters(&senderCalls, &listenerCalls))
		// inner passes — cleanup must stay silent.
	})
	if got := senderCalls.Load(); got != 0 {
		t.Errorf("sender Logs called %d times on success, want 0", got)
	}
	if got := listenerCalls.Load(); got != 0 {
		t.Errorf("listener Logs called %d times on success, want 0", got)
	}
}

func TestDumpTunnelLogsOnFail_EmitsOnFailure(t *testing.T) {
	const helperEnv = "AZTUNNEL_TEST_DUMP_FAILURE"
	if os.Getenv(helperEnv) == "1" {
		var senderCalls, listenerCalls atomic.Int32
		dumpTunnelLogsOnFail(t, tunnelWithLogCounters(&senderCalls, &listenerCalls))
		t.Error("intentional failure to exercise cleanup diagnostics")
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestDumpTunnelLogsOnFail_EmitsOnFailure$", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	output, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected helper test to fail with exit code 1; got %v\n%s", err, output)
	}
	for _, want := range []string{"--- sender[0] logs ---", "sender log", "--- listener[0] logs ---", "listener log"} {
		if !strings.Contains(string(output), want) {
			t.Errorf("missing %q in failure output:\n%s", want, output)
		}
	}
}
