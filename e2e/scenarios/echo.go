package scenarios

import "testing"

// PlainEcho is the zero-config (literal io.Copy) form of WorkloadServer,
// retained as the byte-for-byte transparency oracle used by the
// correctness, reliability, observability, and topology scenarios. It is
// a type alias, so existing call sites that name *PlainEcho (e.g.
// []*PlainEcho fan-out slices) keep working unchanged.
type PlainEcho = WorkloadServer

// StartPlainEcho starts a plain echo server (WorkloadServer in its
// zero-config ServerEcho mode) on a free localhost port. It is stopped by
// t.Cleanup.
//
// Accepts testing.TB so it is callable from every scenario suite. The
// echo behavior is a literal io.Copy(c, c); see WorkloadServer for why
// the zero-config path stays deliberately dumb.
func StartPlainEcho(t testing.TB) *PlainEcho {
	t.Helper()
	return StartWorkloadServer(t, ServerBehavior{})
}
