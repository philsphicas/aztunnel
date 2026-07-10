//go:build windows

package aadssh

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procReplaceFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

func replaceFile(source, destination string) error {
	replaced, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return &os.LinkError{Op: "replace", Old: source, New: destination, Err: err}
	}
	replacement, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return &os.LinkError{Op: "replace", Old: source, New: destination, Err: err}
	}

	result, _, callErr := procReplaceFileW.Call(
		uintptr(unsafe.Pointer(replaced)),
		uintptr(unsafe.Pointer(replacement)),
		0,
		0,
		0,
		0,
	)
	if result != 0 {
		return nil
	}
	if errors.Is(callErr, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(callErr, windows.ERROR_PATH_NOT_FOUND) {
		// ReplaceFileW requires a destination; Rename handles the initial write.
		return os.Rename(source, destination)
	}
	return &os.LinkError{Op: "replace", Old: source, New: destination, Err: callErr}
}
