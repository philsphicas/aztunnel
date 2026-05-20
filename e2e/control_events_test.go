//go:build e2e

package e2e

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// TestControl_Events_AzureSuccessPath drives the happy-path control
// loop against a real Azure Relay and asserts that the operator-
// visible event sequence — control_started, accept_attempted,
// accept_ok — fires on the listener as one accept goes through.
// control_ended is exercised by the unit test
// TestControlSessionID_StableWithinLoop; the listener subprocess is
// torn down via t.Cleanup (SIGKILL), which does not let the process
// emit the deferred control_ended line, so the integration assertion
// stops at accept_ok. Renew events are deliberately not asserted: the
// production renew interval is 45 minutes and the binary does not
// expose a shorter interval; renew coverage lives in the relay-
// package unit test TestControl_HappyPathEvents and in the mockrelay
// fault tests.
//
// One subtest per available auth method (entra, sas) keeps the
// matrix consistent with the rest of the e2e suite.
func TestControl_Events_AzureSuccessPath(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		auth := auth
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
				"--log-level", "debug",
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr(),
				"--log-level", "debug",
			)

			// control_started signals the control loop reached
			// the operational milestone — the same readiness
			// point every other Azure e2e test waits for.
			waitForLog(t, listener, "control_started", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)

			// Drive one round-trip so accept_attempted and
			// accept_ok fire.
			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial sender: %v", err)
			}
			t.Cleanup(func() { _ = conn.Close() })

			payload := []byte("p9-events\n")
			if _, err := conn.Write(payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			buf := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(payload, buf) {
				t.Fatalf("echo mismatch: got %q, want %q", buf, payload)
			}
			_ = conn.Close()

			waitForLog(t, listener, "accept_attempted", 10*time.Second)
			waitForLog(t, listener, "accept_ok", 10*time.Second)
		})
	}
}
