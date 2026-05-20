// Package bridgecause defines sentinel errors that classify why a
// bridge ended. Cancel sites that abort a bridge stamp their sentinel
// via context.WithCancelCause; the bridge resolves the final cause
// via context.Cause(ctx) and surfaces a stable short name through
// Name() so listener/sender bridge-end log lines carry a structured
// cause attribute operators can grep on.
package bridgecause

import (
	"context"
	"errors"
)

// Sentinels covering every cause a bridge end can be classified as.
// Match exactly one of the labels returned by Name. Wrap with %w for
// callers that want richer messages — Name uses errors.Is.
//
// The "Cause" prefix (vs. the conventional "Err") names the runtime
// concept: these sentinels are passed to context.WithCancelCause and
// recovered through context.Cause(ctx); they are not user-facing
// errors operators inspect directly, only via Name's structured-log
// label. Hence the lint override below.
//
//nolint:staticcheck // ST1012: intentional Cause* naming to match cancel-cause runtime semantics.
var (
	// CausePeerClose indicates the remote WebSocket peer closed the
	// bridge: a Close frame (normal or otherwise), an abrupt drop the
	// websocket layer surfaces as a CloseError, or a ws.Write failure
	// that proves the peer is no longer reachable.
	CausePeerClose = errors.New("bridge: peer close")

	// CauseLocalClose indicates the local end of the bridge closed:
	// the TCP target EOF'd, or a local write/read failed because the
	// local socket went away. On the sender side this is the client
	// hanging up; on the listener side this is the upstream target
	// closing.
	CauseLocalClose = errors.New("bridge: local close")

	// CauseUserCancel indicates the bridge ended because the user
	// (or the process) cancelled the parent context. Name(...) also
	// reports user_cancel for plain context.Canceled, so signal-
	// driven shutdown paths surface the same label without callers
	// having to wrap their context with WithCancelCause.
	CauseUserCancel = errors.New("bridge: user cancel")

	// CauseRenewFailure indicates the control channel tore the bridge
	// down because token renewal failed. Stamped by the listener's
	// renewLoop when GetToken or the renewToken write fail.
	CauseRenewFailure = errors.New("bridge: control renew failure")

	// CauseControlError indicates a non-renew control-channel failure
	// (control ping failure, control read failure, bad frame) tore
	// the bridge down. Stamped by the listener's pingLoop and by the
	// control-loop main read on a non-cancel error.
	CauseControlError = errors.New("bridge: control error")

	// CauseTimeout indicates a timeout (idle, read, dial deadline)
	// ended the bridge. Name(...) also reports timeout for plain
	// context.DeadlineExceeded so deadline-driven shutdown paths
	// surface the same label without explicit wrapping.
	CauseTimeout = errors.New("bridge: timeout")

	// CauseUnknown is the fallback when no specific cause was stamped
	// and the context error does not match any classified sentinel.
	CauseUnknown = errors.New("bridge: unknown")
)

// Name returns a short, stable, structured-log-friendly label for
// err: one of peer_close, local_close, user_cancel, renew_failure,
// control_error, timeout, unknown.
//
// Recognised inputs include the bridgecause sentinels (matched via
// errors.Is so wrapped errors work), context.Canceled (user_cancel),
// and context.DeadlineExceeded (timeout). nil and any unrecognised
// error map to unknown.
func Name(err error) string {
	switch {
	case err == nil:
		return "unknown"
	case errors.Is(err, CausePeerClose):
		return "peer_close"
	case errors.Is(err, CauseLocalClose):
		return "local_close"
	case errors.Is(err, CauseUserCancel):
		return "user_cancel"
	case errors.Is(err, CauseRenewFailure):
		return "renew_failure"
	case errors.Is(err, CauseControlError):
		return "control_error"
	case errors.Is(err, CauseTimeout):
		return "timeout"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "user_cancel"
	default:
		return "unknown"
	}
}
