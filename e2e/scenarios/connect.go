package scenarios

import (
	"context"
	"io"
)

// ConnectClient bridges stdio of an aztunnel relay-sender connect
// invocation. Reads come from the sender's stdout; writes go to its
// stdin. Logs returns the sender's stderr captured so far (used by
// failure-mode scenarios that assert error shape + token redaction).
// Wait blocks until the sender exits and returns its exit error (nil
// = clean exit). Close releases all resources; safe to call even if
// the sender has already exited.
//
// Azure backend: Read returns bytes from the sender subprocess's
// stdout, Write goes to its stdin, Logs returns its stderr, Wait
// waits on cmd.Wait and returns its error. Close kills the
// subprocess.
//
// Mock backend: Read drains bytes from the in-process sender's
// stdout pipe, Write feeds the sender's stdin pipe, Logs returns the
// slog text buffer (the in-process sender has no separate stderr),
// Wait blocks until the sender.Connect goroutine returns. Close
// cancels the sender's context and closes both pipes.
//
// Cross-backend assertion shape: scenarios MUST NOT assert on
// CLI-specific behaviors (subprocess stderr exact text, exit codes).
// Both backends populate Logs() with the sender's slog output, so
// assertions like "Logs() does not contain the SAS key" or "Logs()
// contains 'relay dial failed'" work identically.
type ConnectClient interface {
	io.ReadWriteCloser

	// Logs returns the sender's stderr captured since start (Azure)
	// or the slog text buffer of the in-process sender (mock).
	// Returns a snapshot; safe to call concurrently with Read /
	// Write.
	Logs() string

	// Wait blocks until the sender exits or ctx is cancelled.
	// Returns the sender's exit error (nil = exit-0 / clean
	// goroutine return). For in-process mock, returns when
	// sender.Connect() returns. The ctx cancellation does not stop
	// the sender — call Close for that — it only releases Wait's
	// caller.
	Wait(ctx context.Context) error
}
