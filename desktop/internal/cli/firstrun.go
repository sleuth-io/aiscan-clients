package cli

import (
	"os"
	"path/filepath"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/auth"
)

// firstRunMarkerPath is the stamp recording that the daemon's one-shot
// first-run login prompt has been handled. It lives next to the token cache
// (auth.ConfigDir) so it survives updates and is removed by an uninstall.
func firstRunMarkerPath() (string, error) {
	d, err := auth.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "login-prompted"), nil
}

// claimFirstRunLogin reports whether the daemon should start the login flow on
// its own: true only the very first time the daemon runs against this config
// dir with no cached token. On macOS a crowded menu bar can silently hide the
// tray icon, leaving a fresh install with no visible "Log in" to click — this
// one-shot opens the browser approval anyway so the install can start working.
//
// The marker is stamped on the first run whether or not a login is needed, and
// stamped *before* any browser opens, so the prompt can never fire twice — in
// particular, an explicit logout sticks and a marker we fail to write means no
// prompt rather than a prompt on every start.
func claimFirstRunLogin(instance string) bool {
	path, err := firstRunMarkerPath()
	if err != nil {
		return false
	}
	if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
		return false // already stamped, or unreadable — don't risk re-prompting
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false
	}
	if err := os.WriteFile(path, []byte("aiscan's one-time first-run login prompt has run\n"), 0o600); err != nil {
		return false
	}
	_, loggedIn := auth.CachedToken(instance)
	return !loggedIn
}
