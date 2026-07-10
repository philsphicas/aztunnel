//go:build !windows

package aadssh

import "os"

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
