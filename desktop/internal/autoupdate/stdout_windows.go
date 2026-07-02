//go:build windows

package autoupdate

// suppressStdout is a no-op on Windows: go-selfupdate's progress output will
// show, which is acceptable on a secondary platform.
func suppressStdout() func() {
	return func() {}
}
