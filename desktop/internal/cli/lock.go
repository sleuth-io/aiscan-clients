package cli

import (
	"os"
	"path/filepath"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/autoupdate"
)

// daemonLockPath is the single-instance lock file, kept in the same cache dir
// as the update state (so the AISCAN_CACHE_DIR override isolates the lock
// too, e.g. in tests). The directory is created on demand.
func daemonLockPath() (string, error) {
	dir, err := autoupdate.CacheDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.lock"), nil
}
