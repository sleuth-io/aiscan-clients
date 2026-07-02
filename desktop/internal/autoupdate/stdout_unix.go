//go:build !windows

package autoupdate

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// suppressStdout redirects stdout to /dev/null at the file-descriptor level
// and returns a restore function. Needed because go-selfupdate writes progress
// directly to fd 1, bypassing os.Stdout.
func suppressStdout() func() {
	origStdout, err := syscall.Dup(syscall.Stdout)
	if err != nil {
		return func() {}
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		syscall.Close(origStdout)
		return func() {}
	}

	// unix.Dup2 rather than syscall.Dup2 — the latter doesn't exist on
	// linux/arm64.
	if err := unix.Dup2(int(devNull.Fd()), syscall.Stdout); err != nil {
		devNull.Close()
		syscall.Close(origStdout)
		return func() {}
	}
	devNull.Close()

	return func() {
		unix.Dup2(origStdout, syscall.Stdout)
		syscall.Close(origStdout)
	}
}
