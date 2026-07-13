package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/autoupdate"
)

// A macOS tray icon only renders for a process whose LaunchServices
// registration is fresh — an exec-in-place keeps the old image's registration
// and the icon silently never appears. Our own update restarts spawn a
// successor (autoupdate.Respawn), but a *pre-fix* daemon adopting this update
// exec'd over itself, and it left no on-disk trace the new binary could
// detect. So the daemon bounces itself into a fresh process once on the first
// start of each version: cheap, loop-safe via the stamp below, and it heals
// the one generation an old binary exec'd.

// bounceStampPath is the file recording the last version that performed the
// registration bounce. It lives with the other update state in the cache dir.
func bounceStampPath() (string, error) {
	d, err := autoupdate.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "registration-bounce"), nil
}

// claimRegistrationBounce reports whether this daemon start is the first for
// version and should bounce into a fresh process. The stamp is written before
// reporting true, so a bounce can never loop: the relaunched process reads its
// own version back and proceeds. Any failure reads as "don't bounce" — a
// stale registration costs an icon, a bounce loop costs the daemon.
func claimRegistrationBounce(version string) bool {
	path, err := bounceStampPath()
	if err != nil {
		return false
	}
	prev, err := os.ReadFile(path)
	if err == nil && strings.TrimSpace(string(prev)) == version {
		return false
	}
	if err != nil && !os.IsNotExist(err) {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false
	}
	if err := os.WriteFile(path, []byte(version+"\n"), 0o644); err != nil {
		return false
	}
	return true
}

// launchdJobPid matches the running job's pid in `launchctl print` output.
var launchdJobPid = regexp.MustCompile(`(?m)^\s*pid = (\d+)\b`)

// launchdOwnsDaemon reports whether launchd's user agent job is this very
// process. True both when launchd started us and when an old binary exec'd
// over the process launchd started — exec preserves the pid launchd tracks.
// When true, exiting non-zero is the best restart: KeepAlive relaunches a
// fresh process that stays under launchd's crash supervision.
func launchdOwnsDaemon() bool {
	out, err := exec.Command("launchctl", "print",
		fmt.Sprintf("gui/%d/%s", os.Getuid(), launchAgentLabel)).Output()
	if err != nil {
		return false
	}
	return parseLaunchdPid(out) == os.Getpid()
}

// parseLaunchdPid extracts the job's running pid from `launchctl print`
// output, or 0 when the job isn't running (or the format changed — callers
// then fall back to the respawn path, which is safe everywhere).
func parseLaunchdPid(out []byte) int {
	m := launchdJobPid.FindSubmatch(out)
	if m == nil {
		return 0
	}
	pid, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0
	}
	return pid
}
