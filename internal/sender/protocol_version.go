package sender

import "github.com/philsphicas/aztunnel/internal/protocol"

// DefaultSenderMaxProtocolVersion is the default upper bound on the
// relay protocol version this sender will attempt against a listener.
//
// In 0.4.0 this is 1: stream multiplexing (protocol v2) is an opt-in
// capability; operators raise this to 2 via --max-protocol-version=2 or
// AZTUNNEL_MAX_PROTOCOL_VERSION=2 to enable it. In 0.5.0 this constant
// flips to 2 (i.e. protocol.MuxVersion) so mux becomes the default;
// operators wanting to keep the legacy path use --max-protocol-version=1
// to pin the sender to v1.
//
// The 0.5.0 flip is a single-line change to this constant. Every
// downstream surface picks up the new value automatically:
//
//   - Library callers that construct a PortForwardConfig{}/SOCKS5Config{}
//     with the zero value get the new default via
//     NormalizeSenderMaxProtocolVersion(0).
//   - The CLI default surfaced by kong reads this constant via the
//     `${defaultSenderMaxProtocolVersion}` substitution wired in
//     cmd/aztunnel/main.go (see TestKongDefault_MaxProtocolVersion_
//     TracksConstants for the pin).
//   - The protocol_version_test.go pin in this package and the kong
//     pin in cmd/aztunnel will both fail if they aren't updated in
//     the same commit as this constant.
//
// help.go and the README / docs/mux.md prose tables ARE NOT
// automatically updated — they need a matching edit in the same
// commit as the constant flip.
const DefaultSenderMaxProtocolVersion = protocol.CurrentVersion

// NormalizeSenderMaxProtocolVersion clamps a caller-supplied protocol
// version ceiling into the supported range and substitutes the default
// for the zero value (so library callers that never touch the field get
// the same behaviour as a CLI invocation with no flag).
//
// Values below 1 are clamped up to 1 (the protocol's lowest version);
// values above protocol.MuxVersion are clamped down to protocol.MuxVersion
// (a user setting =99 to "future-proof" doesn't get silently broken
// behaviour against a v3 sender we haven't written yet). A logger may
// emit a warn line for either clamp; this helper itself is silent.
func NormalizeSenderMaxProtocolVersion(v int) int {
	if v == 0 {
		return DefaultSenderMaxProtocolVersion
	}
	if v < 1 {
		return 1
	}
	if v > protocol.MuxVersion {
		return protocol.MuxVersion
	}
	return v
}
