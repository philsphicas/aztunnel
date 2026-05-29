package server

import (
	"errors"
	"fmt"
	"sync/atomic"
)

// Option configures a Server constructed via NewServerForTesting. Each
// Option arms a deterministic fault — a close code, a delay, or a
// rejection — that exercises a relay failure mode the in-process
// mock can reproduce reliably but Azure Relay cannot.
//
// Options are testing-only: the production constructor NewServer
// never invokes any Option, so a Server returned from NewServer is
// guaranteed to have every fault knob disarmed regardless of which
// callers (including the aztunnel-relay CLI) import this package.
// The "do not use in production" rule is therefore enforced by code
// path, not by import boundary; callers building production
// binaries MUST construct their Server with NewServer.
type Option func(*Server) error

// faults holds the configured fault-injection state for a Server. It
// must be reachable from request handlers, so the field lives on
// Server. All access is via atomic operations so the read-side cost
// is a single load and no extra synchronization with the rest of the
// server.
//
// Single-shot knobs (closeCodeOnAccept, closeControlOnRenew,
// rejectControlDial) use Swap / CompareAndSwap so the first event
// that observes the armed value also disarms it.
type faults struct {
	closeCodeOnAccept   atomic.Int32 // 0 = disarmed, otherwise the WebSocket close code to emit
	closeControlOnRenew atomic.Bool
	rejectControlDial   atomic.Bool
}

// WithCloseControlOnRenew makes the server close the listener's
// control WebSocket as soon as it observes the next inbound
// renewToken message. The listener's renewLoop will then observe its
// next write or read fail and trigger a reconnect, exercising the
// renew-failure client log path.
//
// This knob replaces the previously-proposed "silent renew failure":
// the on-wire renew protocol is fire-and-forget — the listener does
// NOT expect an echo back from the relay, so a server that silently
// swallows renewToken messages is indistinguishable from a healthy
// one. An observable close is the closest analogue tests can drive
// against the current client.
//
// Single-shot: fires once on the first renewToken observed by any
// listener, then disarms.
func WithCloseControlOnRenew() Option {
	return func(s *Server) error {
		s.faults.closeControlOnRenew.Store(true)
		return nil
	}
}

// WithCloseCodeOnAccept makes the next accept-side (listener-
// rendezvous) WebSocket emit the given close code (for example 1011
// "server error", or an application code in the 3000-4999 range like
// 4400). The peer observes the configured code in its close frame.
//
// Sendable close codes are constrained by RFC 6455 §7.4.1 and the
// coder/websocket library's wire-code validator: only 1000-1003,
// 1007-1014, and 3000-4999 may appear on the wire. Codes outside
// that set — including the reserved 1004 / 1005 / 1006 / 1015 and
// the protocol-reserved range 1016-2999 — are rejected at
// construction with a clear error.
//
// A test that wants the 1006 "abnormal closure" code path on the
// client side must close the underlying TCP socket without a close
// frame instead — there is no Option for that today; add a dedicated
// helper when a test needs it.
//
// Single-shot: fires on the first accept after construction, then
// disarms.
func WithCloseCodeOnAccept(code int) Option {
	return func(s *Server) error {
		if !isSendableCloseCode(code) {
			return fmt.Errorf("WithCloseCodeOnAccept: WS code %d is not sendable in a close frame (sendable: 1000-1003, 1007-1014, 3000-4999)", code)
		}
		s.faults.closeCodeOnAccept.Store(int32(code))
		return nil
	}
}

// isSendableCloseCode mirrors coder/websocket's validWireCloseCode:
// accept 1000-1014 and 3000-4999, except the four codes RFC 6455
// reserves as forbidden on the wire (1004, 1005, 1006, 1015).
func isSendableCloseCode(code int) bool {
	switch code {
	case 1004, 1005, 1006, 1015:
		return false
	}
	if code >= 1000 && code <= 1014 {
		return true
	}
	if code >= 3000 && code <= 4999 {
		return true
	}
	return false
}

// WithRejectControlDial rejects the next inbound listener control-
// channel dial outright at HTTP-upgrade time, returning HTTP 503.
// "Control dial" is the listener-side path: aztunnel listeners open
// the long-lived control WebSocket with the relay via
// /$hc/<entity>?sb-hc-action=listen. The sender side uses the
// rendezvous (accept) endpoints and is unaffected by this knob.
//
// The check fires after SAS validation, so an unauthenticated probe
// cannot accidentally consume the fault. Tests that don't want to
// mint tokens should set Config.SkipAuth.
//
// Single-shot: fires on the first authenticated listen request,
// then disarms.
func WithRejectControlDial() Option {
	return func(s *Server) error {
		s.faults.rejectControlDial.Store(true)
		return nil
	}
}

// NewServerForTesting returns a Server configured with testing-only
// fault-injection hooks. It takes the same Config the production
// constructor takes; faults compose on top of normal behaviour.
// Production code MUST use NewServer instead — the two constructors
// share the same Server type so existing public methods (Handler,
// Serve, ...) continue to work.
//
// Options are applied left-to-right; any option that returns an
// error causes construction to fail.
func NewServerForTesting(cfg Config, opts ...Option) (*Server, error) {
	s, err := NewServer(cfg)
	if err != nil {
		return nil, err
	}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("NewServerForTesting: nil Option")
		}
		if err := opt(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}
