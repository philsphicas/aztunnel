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
// 30s preserves the existing 30s per-attempt dial timeout and permits
// several 1–5s backoff cycles before giving up; longer than any
// reasonable local-app socket timeout, so existing happy-path callers
// see no behaviour change.
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
