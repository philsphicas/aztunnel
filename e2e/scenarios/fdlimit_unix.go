//go:build unix

package scenarios

import "syscall"

// getFDLimit returns the current soft RLIMIT_NOFILE for the process.
// The second return is false on any error so callers treat the check
// as unavailable rather than zero.
func getFDLimit() (uint64, bool) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, false
	}
	return lim.Cur, true
}
