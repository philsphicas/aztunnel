package main_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

// TestSubprocess_EndToEnd builds aztunnel and aztunnel-relay, spawns
// the relay and a listener + port-forward sender as real subprocesses,
// and round-trips bytes through a local echo TCP server. This catches
// CLI/env wiring issues that in-process tests can't (--relay parsing,
// SAS credential resolution, default suffix decisions).
//
// The relay always runs with TLS (a self-signed cert generated on
// startup); aztunnel uses --relay-insecure-tls to accept it. aztunnel
// no longer dials plain ws://, so there is only one variant.
func TestSubprocess_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess build/run in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	echoAddr := startEcho(t, ctx)
	rp := startRelay(t, ctx)

	entity := "subproc-end-to-end"
	_ = startListener(t, ctx, rp.relayURL, entity)

	pfBind := fmt.Sprintf("127.0.0.1:%d", pickFreePort(t))
	_ = startPortForwardSender(t, ctx, rp.relayURL, entity, pfBind, echoAddr)

	waitForTCP(t, pfBind, 5*time.Second)

	// The listener may still be establishing its control WS.
	// The sender's DialWithRetry handles 404s, but it backs off
	// up to ~1s — give it a window.
	conn, err := dialWithRetry(pfBind, 5*time.Second)
	if err != nil {
		t.Fatalf("dial port-forward: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	want := []byte("subprocess round-trip\n")
	conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch:\n got=%q\nwant=%q", got, want)
	}
}
