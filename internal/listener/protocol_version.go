package listener

import "github.com/philsphicas/aztunnel/internal/protocol"

// DefaultListenerMaxProtocolVersion is the default upper bound on the
// relay protocol version this listener will accept.
//
// Stays at protocol.MuxVersion (2) across both 0.4.0 (mux opt-in on
// the sender side) and 0.5.0 (mux on by default): a listener should
// always speak the highest version it implements, so a rolling
// upgrade can land sender-side mux without redeploying listeners.
//
// Operators who need to pin a listener fleet back to v1 (e.g. for a
// v2 emergency rollback) set Config.MaxProtocolVersion = 1, which
// CLI surface --max-protocol-version=1 binds to. No environment
// variable binding — see Config.MaxProtocolVersion.
const DefaultListenerMaxProtocolVersion = protocol.MuxVersion
