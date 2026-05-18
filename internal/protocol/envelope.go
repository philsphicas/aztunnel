// Package protocol defines the wire format for the aztunnel relay protocol.
//
// Every connection through the relay begins with a single JSON envelope
// exchange (one text WebSocket message in each direction), followed by
// raw binary WebSocket frames for data.
package protocol

// ConnectEnvelope is sent by the relay-sender to the relay-listener
// immediately after the rendezvous WebSocket is established.
type ConnectEnvelope struct {
	// Version is the protocol version (currently 1).
	Version int `json:"version"`

	// Target is the host:port the sender wants the listener to dial.
	Target string `json:"target"`

	// Metadata carries extensible key-value pairs for future use
	// (auth tokens, compression negotiation, trace IDs, etc.).
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ConnectResponse is sent by the relay-listener back to the relay-sender
// after attempting to dial the requested target.
type ConnectResponse struct {
	// Version is the protocol version (currently 1).
	Version int `json:"version"`

	// OK is true if the listener successfully connected to the target.
	OK bool `json:"ok"`

	// Error is a human-readable error message if OK is false.
	// Must not leak internal details (IPs, paths, etc.).
	Error string `json:"error,omitempty"`

	// Code is a machine-readable error classification when OK is false.
	// Empty when OK is true or when the listener could not classify the
	// failure. Senders treat unknown values the same as an empty string
	// (generic failure). Optional and backward-compatible: senders and
	// listeners pinned to earlier versions will ignore it.
	Code string `json:"code,omitempty"`
}

// CurrentVersion is the current protocol version.
const CurrentVersion = 1

// Connection-failure codes carried in ConnectResponse.Code. Used to map
// listener-side dial failures to client-visible status (e.g. SOCKS5 REP
// bytes). The set is intentionally small; new categories should only be
// added when they map to a distinct user-visible outcome.
const (
	// CodeConnectionRefused indicates the target actively refused the
	// connection (TCP RST), e.g. nothing is listening on the port.
	CodeConnectionRefused = "connection_refused"

	// CodeHostUnreachable indicates the target host is reachable on the
	// network but the route to it failed (ICMP host-unreachable, ARP
	// failure, etc.).
	CodeHostUnreachable = "host_unreachable"

	// CodeNetworkUnreachable indicates the target's network is not
	// reachable from the listener.
	CodeNetworkUnreachable = "network_unreachable"

	// CodeTimeout indicates the dial did not complete within the
	// listener's configured connect timeout (no SYN-ACK from the
	// target, typical of black-holed addresses).
	CodeTimeout = "timeout"
)
