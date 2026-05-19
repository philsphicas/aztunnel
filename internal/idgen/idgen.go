// Package idgen mints short opaque identifiers used as log
// correlation keys across the aztunnel observability surface
// (bridge_id today; listener_id / control_session_id / accept_id in
// later observability work).
//
// IDs are 16 characters of base32 with no padding, encoding 80 bits
// of entropy from crypto/rand. The character set is [A-Z2-7], which
// is safe to embed in slog TextHandler output without quoting and is
// easy to copy/paste from log lines.
package idgen

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"io"
)

// bridgeIDBytes is the number of random bytes encoded into a
// BridgeID. 10 bytes encode to exactly 16 base32 characters with no
// padding (10*8 == 16*5).
const bridgeIDBytes = 10

// NewBridgeID returns a fresh, opaque correlation ID for a single
// bridge. It panics if the OS RNG fails, which only happens under
// resource exhaustion so catastrophic that the process cannot
// continue anyway — propagating an error from every call site would
// pollute the public API for a failure mode callers cannot recover
// from.
func NewBridgeID() string {
	var b [bridgeIDBytes]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		panic(fmt.Errorf("idgen: read crypto/rand: %w", err))
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
}
