//go:build !unix

package relayparity

// getFDLimit on non-Unix platforms (Windows, plan9, js) returns
// (0, false) so skipIfFDLimitTooLow becomes a no-op there. Windows
// has its own per-process handle limits but no analogue of
// RLIMIT_NOFILE; tightening the cross-platform behaviour is left for
// a future change if Windows actually starts running these
// benchmarks.
func getFDLimit() (uint64, bool) {
	return 0, false
}
