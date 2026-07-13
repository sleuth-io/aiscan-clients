//go:build windows

package cli

import (
	"log"

	"golang.org/x/sys/windows"
)

// acquireDaemonLock takes the single-instance lock via an exclusive
// (no-share-mode) file handle, Windows' equivalent of a flock that a crashed
// process can never leave stuck. locked=false means another daemon holds it.
func acquireDaemonLock() (release func(), locked bool, err error) {
	lockPath, err := daemonLockPath()
	if err != nil {
		return nil, false, err
	}
	path, err := windows.UTF16PtrFromString(lockPath)
	if err != nil {
		return nil, false, err
	}
	h, err := windows.CreateFile(path, windows.GENERIC_WRITE, 0 /* no sharing */, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err == windows.ERROR_SHARING_VIOLATION {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return func() { _ = windows.CloseHandle(h) }, true, nil
}

// takeOverIncumbent is unix-only (there is no invisible-tray failure mode to
// heal on Windows); callers gate on GOOS, this stub only satisfies the build.
func takeOverIncumbent(logger *log.Logger) (release func(), locked bool) {
	return nil, false
}
