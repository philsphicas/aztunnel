//go:build windows

package listener

import "syscall"

// Winsock errors use WSA values; syscall's portable errno constants are
// synthetic on Windows and do not match errors returned by net.Dial.
var (
	errConnectionRefused  error = syscall.Errno(10061) // WSAECONNREFUSED
	errHostUnreachable    error = syscall.Errno(10065) // WSAEHOSTUNREACH
	errNetworkUnreachable error = syscall.Errno(10051) // WSAENETUNREACH
)
