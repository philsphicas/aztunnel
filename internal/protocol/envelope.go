// Package protocol defines the wire format for the aztunnel relay protocol.
//
// # v1 (single connection)
//
// The sender sends one ConnectEnvelope as a WebSocket text message; the
// listener replies with one ConnectResponse as a WebSocket text message;
// from then on the WebSocket carries raw binary frames for data.
//
// # v2 (multiplexed)
//
// The sender sends a MuxHandshake as a WebSocket text message; the listener
// replies with a ConnectResponse as a WebSocket text message. The two sides
// then switch to an smux session over the WebSocket (wrapped as a
// net.Conn). Each smux stream carries an independent target connection:
// the sender writes a length-prefixed ConnectEnvelope, the listener writes
// a length-prefixed ConnectResponse, and the rest of the stream is raw
// target bytes.
//
// Per-stream envelopes are length-prefixed (not newline-delimited or
// json.Decoder-buffered) so that a bufio/json reader cannot accidentally
// consume bytes that arrive immediately after the response — whether a
// server-first banner (SSH "SSH-2.0…", SMTP "220…") or a client-first
// startup payload (e.g. Postgres, HTTP).
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// ConnectEnvelope is sent by the relay-sender to the relay-listener at the
// start of every target connection: as a WebSocket text message in v1, and
// as the first length-prefixed frame on each smux stream in v2.
type ConnectEnvelope struct {
	// Version is the protocol version (currently 1).
	Version int `json:"version"`

	// Target is the host:port the sender wants the listener to dial.
	Target string `json:"target"`

	// Metadata carries extensible key-value pairs for future use
	// (auth tokens, compression negotiation, trace IDs, etc.).
	Metadata map[string]string `json:"metadata,omitempty"`

	// BridgeID is a sender-minted opaque identifier for this bridge.
	// Listeners bind it into their request-scoped logger so logs on
	// both sides correlate via grep for the same value. Listeners
	// receiving an empty value MUST NOT mint a fallback; absence
	// means a sender on a pre-P5 version. The format is unspecified
	// (callers should not parse).
	BridgeID string `json:"bridge_id,omitempty"`
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

	// ListenerID is a listener-minted opaque identifier for the
	// listener process. Stable for the lifetime of one listener
	// process; changes on restart. Senders log it on receipt;
	// mixed-version senders ignore the field. Format is unspecified
	// (currently 16 base32 chars from [A-Z2-7]; do not parse).
	ListenerID string `json:"listener_id,omitempty"`
}

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

	// CodeDNSNotFound indicates the listener could not resolve the target
	// hostname (typically NXDOMAIN or NODATA). Distinct from CodeHostUnreachable
	// because the failure is at the name-resolution layer, not the network.
	CodeDNSNotFound = "dns_not_found"

	// CodeDNSTimeout indicates DNS resolution exceeded the resolver's deadline.
	// Distinct from CodeTimeout because the failure happened before any SYN was
	// sent; the underlying network may be fine.
	CodeDNSTimeout = "dns_timeout"
)

// MuxHandshake is sent by the sender as the FIRST WebSocket message on a
// relay rendezvous in v2 (multiplexed) mode. The listener inspects Version
// and Mode via FirstMessage and replies with a ConnectResponse.
type MuxHandshake struct {
	// Version is the protocol version (MuxVersion).
	Version int `json:"version"`

	// Mode identifies this as a mux handshake (MuxMode).
	Mode string `json:"mode"`

	// Capabilities carries optional, forward-compatible negotiation
	// hints. Unknown fields must be ignored by the receiver.
	Capabilities *MuxCapabilities `json:"capabilities,omitempty"`
}

// MuxCapabilities carries forward-compatible negotiation hints in a
// MuxHandshake. Receivers must ignore unknown fields and tolerate missing
// ones (any sub-field may be zero).
type MuxCapabilities struct {
	// ClientVersion is a free-form identifier of the sender build
	// (e.g. "aztunnel/0.3.0"). Informational only.
	ClientVersion string `json:"clientVersion,omitempty"`

	// KeepAliveSeconds is the sender's smux keepalive interval in seconds.
	// The listener uses this only as a hint; it sets its own values.
	KeepAliveSeconds int `json:"keepAliveSeconds,omitempty"`

	// MaxStreams is the maximum number of concurrent streams the sender
	// will open on this session. Informational; the listener still enforces
	// its own cap.
	MaxStreams int `json:"maxStreams,omitempty"`
}

// FirstMessage is used by the listener to inspect the version and mode of
// the first WebSocket message before deciding how to handle the connection.
// JSON is forward-compatible: unknown fields are ignored, so both v1
// ConnectEnvelope and v2 MuxHandshake parse cleanly into this type.
type FirstMessage struct {
	Version int    `json:"version"`
	Mode    string `json:"mode,omitempty"`
}

const (
	// CurrentVersion is the v1 (single-connection) protocol version.
	CurrentVersion = 1

	// MuxVersion is the v2 (multiplexed) protocol version.
	MuxVersion = 2

	// MuxMode is the value of MuxHandshake.Mode that identifies mux mode.
	MuxMode = "mux"
)

// IsMux reports whether the first message indicates v2 mux mode.
func (f FirstMessage) IsMux() bool {
	return f.Version == MuxVersion && f.Mode == MuxMode
}

// maxStreamFrameSize bounds a single length-prefixed stream frame. It is
// generous (envelopes are small JSON objects, typically well under 1 KiB)
// but strict enough to prevent a malicious or corrupt peer from forcing
// huge allocations or stalling the bridge. Must fit in uint16.
const maxStreamFrameSize = 8 * 1024

// WriteStreamEnvelope writes a ConnectEnvelope to an smux stream using
// 2-byte big-endian length-prefixed framing. The framing is what
// matters: it lets the receiver (ReadStreamEnvelope) consume exactly
// the envelope bytes and no more — see ReadStreamResponse for the
// banner-overread rationale this avoids.
func WriteStreamEnvelope(w io.Writer, env ConnectEnvelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	return writeLengthPrefixed(w, data)
}

// ReadStreamEnvelope reads a length-prefixed ConnectEnvelope.
func ReadStreamEnvelope(r io.Reader) (ConnectEnvelope, error) {
	data, err := readLengthPrefixed(r)
	if err != nil {
		return ConnectEnvelope{}, fmt.Errorf("read envelope: %w", err)
	}
	var env ConnectEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return ConnectEnvelope{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	return env, nil
}

// WriteStreamResponse writes a ConnectResponse to an smux stream using
// 2-byte big-endian length-prefixed framing.
func WriteStreamResponse(w io.Writer, resp ConnectResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	return writeLengthPrefixed(w, data)
}

// ReadStreamResponse reads a length-prefixed ConnectResponse. The
// length prefix is what matters: it lets us consume exactly the
// response bytes and no more, so a target banner that arrives
// immediately after the response on a server-first protocol (SSH
// "SSH-2.0...", SMTP "220 ...", etc.) cannot be lost into a decoder's
// internal buffer. The regression test for this lives in
// envelope_test.go:TestReadStreamResponse_NoOverreadOfBanner.
func ReadStreamResponse(r io.Reader) (ConnectResponse, error) {
	data, err := readLengthPrefixed(r)
	if err != nil {
		return ConnectResponse{}, fmt.Errorf("read response: %w", err)
	}
	var resp ConnectResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return ConnectResponse{}, fmt.Errorf("unmarshal response: %w", err)
	}
	return resp, nil
}

func writeLengthPrefixed(w io.Writer, data []byte) error {
	if len(data) > maxStreamFrameSize {
		return fmt.Errorf("frame too large: %d > %d", len(data), maxStreamFrameSize)
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("write length: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
}

func readLengthPrefixed(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("read length: %w", err)
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n == 0 {
		return nil, fmt.Errorf("empty frame")
	}
	if n > maxStreamFrameSize {
		return nil, fmt.Errorf("frame too large: %d > %d", n, maxStreamFrameSize)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("read payload: %w", err)
	}
	return buf, nil
}
