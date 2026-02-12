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
}

// CurrentVersion is the current protocol version.
const CurrentVersion = 1
