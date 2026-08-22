package sender

import "time"

// defaultDialBudget bounds the per-connection relay dial + retry
// duration. Without a per-connection bound, DialWithRetry only
// observes the process-lifetime context, so a sender that keeps
// retrying after the local app has closed its socket will complete
// the rendezvous when a listener eventually appears — and the
// listener will then dial its target for an app that's gone. See
// issue #94 ("ghost rendezvous").
//
// 30s preserves the sender's existing total connection budget. Relay
// handshake attempts and their exponential backoffs all fit inside this
// bound, so a stalled attempt can recover without extending how long the
// local app socket waits.
const defaultDialBudget = 30 * time.Second

// dialBudget returns d unchanged, or defaultDialBudget when d is
// zero (the conventional "unset" marker for time.Duration fields on
// the sender package's *Config types). Negative values also fall
// back to the default rather than yielding an already-expired
// context.
func dialBudget(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultDialBudget
	}
	return d
}
