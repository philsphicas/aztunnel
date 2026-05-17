package relayparity

import (
	"testing"
)

// SenderMode selects which aztunnel sender to bring up.
type SenderMode int

const (
	// ModePortForward starts a relay-sender port-forward bound to
	// SetupOptions.Target. Dialing the resulting SenderAddr is
	// equivalent to dialing Target directly through the tunnel.
	ModePortForward SenderMode = iota
	// ModeSOCKS5 starts a relay-sender socks5-proxy. The resulting
	// SenderAddr speaks SOCKS5; each client connection chooses its own
	// target via the SOCKS5 handshake.
	ModeSOCKS5
)

func (m SenderMode) String() string {
	switch m {
	case ModePortForward:
		return "port-forward"
	case ModeSOCKS5:
		return "socks5"
	default:
		return "unknown"
	}
}

// Backend is the abstraction parity scenarios run against. Each
// implementation knows how to bring up a topology that satisfies
// SetupOptions and to tear it down cleanly via t.Cleanup.
type Backend interface {
	// Name identifies the backend in test output (e.g. "mock",
	// "azure-entra", "azure-sas"). Used to make sub-test paths
	// readable.
	Name() string

	// Setup brings up a relay topology described by opts and returns a
	// Tunnel handle the scenario can drive. All resources (goroutines,
	// subprocesses, listeners, sockets) are registered for cleanup on
	// t via t.Cleanup; the scenario does not have to release them.
	//
	// Setup must block until the topology is ready: the sender's bind
	// address is accepting connections and every requested listener is
	// reachable (control channel established for subprocess backends;
	// goroutine running for in-process). Scenarios assume the tunnel
	// is fully connected on return.
	Setup(t *testing.T, opts SetupOptions) *Tunnel
}

// SetupOptions configures the topology a backend should create.
type SetupOptions struct {
	// NumListeners is the count of listener processes/goroutines to
	// start against the same entity. Must be >= 1.
	NumListeners int

	// SenderMode picks port-forward vs SOCKS5.
	SenderMode SenderMode

	// Target is the dial target for port-forward mode. Ignored for
	// SOCKS5 (clients choose their own target via the SOCKS5
	// handshake).
	Target string

	// AllowedTargets is the listener --allow value(s). Empty means
	// allow all; tests usually pass the addresses of the target
	// servers they started. Slice order is not significant.
	AllowedTargets []string

	// MaxConnections caps concurrent target dials on each listener
	// (`aztunnel relay-listener --max-connections=N`). 0 means
	// unlimited.
	MaxConnections int
}

// Tunnel is a running listener/sender/relay topology returned by
// Backend.Setup. Scenarios drive it by dialing SenderAddr from client
// goroutines.
//
// Per-listener handles (for distribution, hot-drop, hot-add scenarios)
// will be added when the topology suite that needs them lands.
type Tunnel struct {
	// SenderAddr is the host:port clients dial. For ModePortForward
	// this is a plain TCP target that forwards to SetupOptions.Target.
	// For ModeSOCKS5 this is a SOCKS5 proxy that accepts any allowed
	// target via the SOCKS5 handshake.
	SenderAddr string
}
