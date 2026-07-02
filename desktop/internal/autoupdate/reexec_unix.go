//go:build !windows

package autoupdate

import (
	"os"
	"syscall"
)

// Reexec replaces the current process image with the binary at exePath so the
// invocation that just applied a pending update runs on the new version with
// the same args and environment. exePath must be resolved with os.Executable()
// *before* ApplyPendingUpdate — after the swap, /proc/self/exe points at a
// deleted inode. Returns only on failure, in which case the caller simply
// continues on the old image.
func Reexec(exePath string) {
	if exePath == "" {
		return
	}
	_ = syscall.Exec(exePath, os.Args, os.Environ())
}
