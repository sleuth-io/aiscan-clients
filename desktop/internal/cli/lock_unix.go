//go:build !windows

package cli

import (
	"log"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// acquireDaemonLock takes the single-instance flock so a double-click while
// the login-item daemon is already running doesn't spawn a second tray icon.
// locked=false means another daemon holds it. The lock dies with the process,
// so a crash can never leave it stuck.
func acquireDaemonLock() (release func(), locked bool, err error) {
	path, err := daemonLockPath()
	if err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
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
	// Record our pid so a later launch can take the daemon over (see
	// takeOverIncumbent); the held flock is what proves the pid is current.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	return func() { _ = f.Close() }, true, nil
}

// readDaemonLockPid returns the pid recorded in the lock file, or 0 when
// there is none — no daemon ever ran, or the incumbent predates pid
// recording. Only meaningful while the flock is held: the lock proves its
// writer is still alive, so the pid cannot have been recycled.
func readDaemonLockPid() int {
	path, err := daemonLockPath()
	if err != nil {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// takeOverIncumbent terminates the daemon holding the single-instance lock
// and takes the lock for this process. Used when a fresh launch finds a
// daemon already running: the incumbent may be an older binary, or invisible
// under a stale LaunchServices registration, and the user's launch is the
// instruction to replace it. locked=false means the incumbent is unknown (a
// pre-pid-recording version) or would not exit.
func takeOverIncumbent(logger *log.Logger) (release func(), locked bool) {
	pid := readDaemonLockPid()
	if pid == 0 || pid == os.Getpid() {
		return nil, false
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return nil, false
	}
	logger.Printf("asked the running daemon (pid %d) to exit so this launch can take over", pid)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if r, ok, err := acquireDaemonLock(); err == nil && ok {
			return r, true
		}
	}
	logger.Printf("daemon (pid %d) did not exit; giving up the takeover", pid)
	return nil, false
}
