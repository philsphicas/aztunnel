package bridgecause

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestName_NilIsUnknown(t *testing.T) {
	if got := Name(nil); got != "unknown" {
		t.Errorf("Name(nil) = %q, want %q", got, "unknown")
	}
}

func TestName_Sentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"PeerClose", CausePeerClose, "peer_close"},
		{"LocalClose", CauseLocalClose, "local_close"},
		{"UserCancel", CauseUserCancel, "user_cancel"},
		{"RenewFailure", CauseRenewFailure, "renew_failure"},
		{"ControlError", CauseControlError, "control_error"},
		{"Timeout", CauseTimeout, "timeout"},
		{"Unknown", CauseUnknown, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Name(tc.err); got != tc.want {
				t.Errorf("Name(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestName_Wrapped(t *testing.T) {
	// errors.Is unwrapping must keep classification stable so callers
	// can wrap with %w for context without losing the cause label.
	wrapped := fmt.Errorf("renew at attempt 3: %w", CauseRenewFailure)
	if got := Name(wrapped); got != "renew_failure" {
		t.Errorf("Name(wrapped renew) = %q, want %q", got, "renew_failure")
	}
}

func TestName_DoubleWrapped(t *testing.T) {
	inner := fmt.Errorf("get token: %w", CauseRenewFailure)
	outer := fmt.Errorf("control loop: %w", inner)
	if got := Name(outer); got != "renew_failure" {
		t.Errorf("Name(double wrapped) = %q, want %q", got, "renew_failure")
	}
}

func TestName_StdlibCanceledIsUserCancel(t *testing.T) {
	// The production CLI's signal.NotifyContext uses plain
	// context.WithCancel, so the descendant bridge ctx ends up with
	// context.Canceled (no cause). Name must surface user_cancel for
	// that case so operator-visible logs carry the intended label.
	if got := Name(context.Canceled); got != "user_cancel" {
		t.Errorf("Name(context.Canceled) = %q, want %q", got, "user_cancel")
	}
}

func TestName_StdlibDeadlineExceededIsTimeout(t *testing.T) {
	if got := Name(context.DeadlineExceeded); got != "timeout" {
		t.Errorf("Name(context.DeadlineExceeded) = %q, want %q", got, "timeout")
	}
}

func TestName_WrappedStdlibCanceledIsUserCancel(t *testing.T) {
	wrapped := fmt.Errorf("ws.Read: %w", context.Canceled)
	if got := Name(wrapped); got != "user_cancel" {
		t.Errorf("Name(wrapped Canceled) = %q, want %q", got, "user_cancel")
	}
}

func TestName_UnclassifiedError(t *testing.T) {
	if got := Name(errors.New("synthetic")); got != "unknown" {
		t.Errorf("Name(synthetic) = %q, want %q", got, "unknown")
	}
}

func TestSentinels_AllDistinct(t *testing.T) {
	// Defensive against future copy-paste regressions: every sentinel
	// must be a distinct identity so errors.Is classification stays
	// unambiguous.
	sentinels := []error{
		CausePeerClose, CauseLocalClose, CauseUserCancel,
		CauseRenewFailure, CauseControlError, CauseTimeout, CauseUnknown,
	}
	for i := range sentinels {
		for j := i + 1; j < len(sentinels); j++ {
			if errors.Is(sentinels[i], sentinels[j]) {
				t.Errorf("sentinel %d and %d are not distinct", i, j)
			}
		}
	}
}
