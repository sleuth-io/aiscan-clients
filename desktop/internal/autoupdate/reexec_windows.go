//go:build windows

package autoupdate

import "errors"

// Reexec is a no-op on Windows, which has no exec-style process replacement:
// the current run finishes on the old binary and the next invocation picks up
// the swapped-in version.
func Reexec(exePath string) {}

// Respawn always errors on Windows; restart-by-successor is a macOS need (see
// reexec_unix.go) and no caller reaches it here — Reexec's no-op contract
// (adopt on next launch) covers Windows.
func Respawn(exePath string) error {
	return errors.New("respawn: not supported on windows")
}
