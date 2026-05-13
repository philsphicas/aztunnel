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
// --relay-auth handling, default scheme/suffix decisions).
//
// Two variants: plain HTTP (ws://) and self-signed TLS (wss:// with
// --relay-insecure-tls on the client). The other subprocess tests in
// this package run plain HTTP only — TLS is exercised here for
// transport-level coverage, not duplicated across every scenario.
func TestSubprocess_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess build/run in -short mode")
	}

	for _, tc := range []struct {
		name         string
		relayFlags   []string
		clientScheme string // "ws" or "wss"
		extraClient  []string
	}{
		{name: "plain_ws", clientScheme: "ws"},
		{
			name:         "self_signed_tls",
			relayFlags:   []string{"--tls"},
			clientScheme: "wss",
			extraClient:  []string{"--relay-insecure-tls"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			echoAddr := startEcho(t, ctx)
			rp := startRelay(t, ctx, tc.relayFlags...)

			// For TLS variant we drive a wss:// scheme via the
			// port-present heuristic ("host:port" → wss).
			relayFlag := rp.relayURL
			if tc.clientScheme == "wss" {
				relayFlag = rp.addr
			}

			entity := "subproc-" + tc.name
			_ = startListener(t, ctx, relayFlag, entity, tc.extraClient...)

			pfBind := fmt.Sprintf("127.0.0.1:%d", pickFreePort(t))
			_ = startPortForwardSender(t, ctx, relayFlag, entity, pfBind, echoAddr, tc.extraClient...)

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
		})
	}
}
