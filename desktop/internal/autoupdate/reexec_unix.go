//go:build !windows

package autoupdate

import (
	"errors"
	"os"
	"os/exec"
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

// Respawn starts a fresh copy of the binary at exePath with this process's
// arguments, environment, and standard streams; the caller exits after a nil
// return. The child gets its own session (Setsid) so it survives launchd
// tearing down the exiting job's process group.
//
// This is how the macOS daemon adopts an update. Exec-in-place keeps the pid
// but leaves the process's LaunchServices registration pointing at the dead
// image, and the re-exec'd tray icon silently never appears — a status item
// needs a freshly launched process. Reexec remains right for CLI verbs (the
// terminal session stays coherent) and for Linux (supervisors track the pid;
// nothing graphical to lose).
func Respawn(exePath string) error {
	if exePath == "" {
		return errors.New("respawn: no executable path")
	}
	cmd := exec.Command(exePath, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
