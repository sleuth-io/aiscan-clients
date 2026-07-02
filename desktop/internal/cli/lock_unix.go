//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"syscall"
)

// acquireDaemonLock takes the single-instance flock so a double-click while
// the login-item daemon is already running doesn't spawn a second tray icon.
// locked=false means another daemon holds it. The lock dies with the process,
// so a crash can never leave it stuck.
func acquireDaemonLock() (release func(), locked bool, err error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, false, err
	}
	dir = filepath.Join(dir, "aiscan")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { _ = f.Close() }, true, nil
}
