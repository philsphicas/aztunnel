package relay

// Event names emitted by runControlLoop and its goroutines. Each event
// is the msg of one slog record so operators can filter mechanically
// with msg="<name>". Every record additionally carries the
// control_session_id attribute bound in runControlLoop.
const (
	EventControlStarted  = "control_started"
	EventControlEnded    = "control_ended"
	EventRenewAttempted  = "renew_attempted"
	EventRenewOK         = "renew_ok"
	EventRenewFailed     = "renew_failed"
	EventAcceptAttempted = "accept_attempted"
	EventAcceptOK        = "accept_ok"
	EventAcceptDropped   = "accept_dropped"
)

// renew_failed.code values. The on-wire renew protocol is fire-and-
// forget: the listener does NOT read an echo after sending
// renewToken, so the detectable renew failures are connection-level
// (write fails, next read fails) or local (token fetch error,
// context cancellation). RenewFailedAuthFailed is reserved for
// TokenProvider errors that the provider itself classifies as
// authentication; the runtime does not classify by string heuristics
// and emits this code only when a caller passes a typed auth error.
const (
	RenewFailedConnectionLost = "connection_lost"
	RenewFailedAuthFailed     = "auth_failed"
	RenewFailedContextCancel  = "context_cancelled"
	RenewFailedTokenFetchFail = "token_fetch_failed"
)

// accept_dropped.reason values.
const (
	AcceptDroppedSemaphoreFull = "semaphore_full"
	AcceptDroppedDialFailed    = "dial_failed"
	AcceptDroppedAuthFailed    = "auth_failed"
)

// control_ended.reason values. A small enum so an operator query
// ("give me every loop that ended because dial failed") matches one
// fixed string. Forced-reconnect causes (renew_failed, ping_failed)
// are surfaced here separately from the read-loop's wrapped
// context.Canceled return — see runControlLoop's endCause tracking.
const (
	ControlEndedDialFailed       = "dial_failed"
	ControlEndedTokenFetchFailed = "token_fetch_failed"
	ControlEndedAuthFailed       = "auth_failed"
	ControlEndedReadFailed       = "read_failed"
	ControlEndedContextCancelled = "context_cancelled"
	ControlEndedRenewFailed      = "renew_failed"
	ControlEndedPingFailed       = "ping_failed"
)
