// Package idgen mints short opaque identifiers used as log
// correlation keys across the aztunnel observability surface
// (bridge_id, listener_id, control_session_id; future accept_id and
// related ids will join here).
//
// IDs are not security-sensitive — they exist to correlate
// observations across components. All ids share one shape: 16
// base32 chars from the [A-Z2-7] charset, encoded with no padding
// from 10 bytes (80 bits) of crypto/rand. Sharing the shape keeps
// the log surface uniform and lets operators grep with one regex.
package idgen

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"io"
)

// bridgeIDBytes is the number of random bytes encoded into every
// observability id minted in this package. 10 bytes encode to
// exactly 16 base32 characters with no padding (10*8 == 16*5).
const bridgeIDBytes = 10

// NewBridgeID returns a fresh, opaque correlation ID for a single
// bridge. It panics if the OS RNG fails, which only happens under
// resource exhaustion so catastrophic that the process cannot
// continue anyway — propagating an error from every call site would
// pollute the public API for a failure mode callers cannot recover
// from.
func NewBridgeID() string {
	return newID()
}

// NewListenerID returns a fresh opaque listener identifier. It is
// intended to be called exactly once per listener process at
// startup; the returned value is stable for the lifetime of that
// process and changes on restart, so senders that log the field
// can mechanically detect "the listener I'm talking to today is a
// different process than yesterday". Panics on OS RNG failure
// (see NewBridgeID).
func NewListenerID() string {
	return newID()
}

// NewControlSessionID returns a fresh, opaque correlation ID for a
// single run of the relay control loop. Callers bind the result onto
// their slog logger with `logger.With("control_session_id", id)` so
// every log line emitted during the session carries the tag — when
// the control channel dies and a fresh loop starts, the next call
// mints a new id, letting operators mechanically split before-and-
// after log streams. Panics on OS RNG failure (see NewBridgeID).
func NewControlSessionID() string {
	return newID()
}

// newID mints one 16-character base32-NoPadding identifier from
// bridgeIDBytes bytes of crypto/rand. All public id constructors
// share this impl because every observability id in aztunnel uses
// the same shape — different exported names exist only so call
// sites read at the right level of intent.
func newID() string {
	var b [bridgeIDBytes]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		panic(fmt.Errorf("idgen: read crypto/rand: %w", err))
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
}
