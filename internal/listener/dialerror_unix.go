//go:build !windows

package listener

import "syscall"

var (
	errConnectionRefused  error = syscall.ECONNREFUSED
	errHostUnreachable    error = syscall.EHOSTUNREACH
	errNetworkUnreachable error = syscall.ENETUNREACH
)
