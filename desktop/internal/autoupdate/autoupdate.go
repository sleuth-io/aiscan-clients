// Package autoupdate keeps the aiscan binary current from GitHub releases
// (tags desktop-vX.Y.Z), ported from the sx CLI's two-phase design:
//
//  1. On every run a background goroutine — throttled to once per day — asks
//     GitHub for the latest desktop release. If there is a newer one it writes
//     a pending-update marker *before* downloading, then swaps the binary in
//     place. The process usually exits before the download finishes; the
//     marker is what survives.
//  2. The next run sees the marker, applies the update synchronously (with a
//     short timeout), and re-execs so that invocation already runs the new
//     version.
//
// Downloads are sha256-verified against the release's checksums.txt. Dev
// builds never update, and AISCAN_DISABLE_AUTOUPDATER=1 turns the whole
// mechanism off (the explicit `aiscan update` verb still works).
package autoupdate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/creativeprojects/go-selfupdate"

	"github.com/sleuth-io/aiscan-clients/desktop/internal/buildinfo"
)

const (
	// GithubOwner and GithubRepo identify the release source; exported for
	// the `aiscan update` verb.
	GithubOwner = "sleuth-io"
	GithubRepo  = "aiscan-clients"

	// ChecksumsFile is the sha256 manifest published with every desktop
	// release; downloads are verified against it.
	ChecksumsFile = "checksums.txt"

	checkInterval     = 24 * time.Hour
	updateCacheFile   = "last-update-check"
	pendingUpdateFile = "pending-update.json"
	updateTimeout     = 30 * time.Second

	disableEnv = "AISCAN_DISABLE_AUTOUPDATER"
)

// pendingUpdate is the phase-1 → phase-2 handoff marker.
type pendingUpdate struct {
	Version   string `json:"version"`
	AssetURL  string `json:"asset_url"`
	AssetName string `json:"asset_name"`
}

// Disabled reports whether autoupdate is turned off via AISCAN_DISABLE_AUTOUPDATER.
func Disabled() bool {
	switch os.Getenv(disableEnv) {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	}
	return false
}

// describeAhead matches a version stamped from a commit past the release tag
// (`git describe` → e.g. 0.2.2-1-g2eb714a). Semver reads that suffix as a
// prerelease of 0.2.2 — *below* the released 0.2.2 — so without this the
// updater would "upgrade" such a build back to the official release.
var describeAhead = regexp.MustCompile(`-\d+-g[0-9a-f]+$`)

// isDevBuild reports whether this binary was built outside the release
// pipeline (plain `go build`, a dirty tree, or a commit past the latest
// release tag) and so must never self-update.
func isDevBuild() bool {
	v := buildinfo.Version
	return v == "dev" || v == "" || strings.Contains(v, "-dirty") || describeAhead.MatchString(v)
}

// CacheDir returns the directory holding autoupdate state, honoring the
// AISCAN_CACHE_DIR override (tests) like auth's AISCAN_CONFIG_DIR. Exported
// for update-adjacent state kept by other packages (the daemon's per-version
// registration-bounce stamp).
func CacheDir() (string, error) {
	if dir := os.Getenv("AISCAN_CACHE_DIR"); dir != "" {
		return dir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "aiscan"), nil
}

func pendingUpdatePath() (string, error) {
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, pendingUpdateFile), nil
}

// Repository returns the GitHub repository the updater watches.
func Repository() selfupdate.Repository {
	return selfupdate.ParseSlug(GithubOwner + "/" + GithubRepo)
}

// NewUpdater builds the configured updater: GitHub source, downloads verified
// against the release's checksums manifest. The default os/arch asset matching
// also skips this repo's browser-extension releases (their .crx/.xpi assets
// never match a `<os>_<arch>` suffix).
func NewUpdater() (*selfupdate.Updater, error) {
	source, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{})
	if err != nil {
		return nil, err
	}
	return selfupdate.NewUpdater(selfupdate.Config{
		Source:    source,
		Validator: &selfupdate.ChecksumValidator{UniqueFilename: ChecksumsFile},
	})
}

// ApplyPendingUpdate checks for a pending update marker and applies it.
// Call at startup, before CheckAndUpdateInBackground; the fast path (no
// marker) is a single os.Stat. Returns true if an update was applied — the
// caller should then Reexec so this invocation runs the new binary.
func ApplyPendingUpdate() bool {
	if Disabled() || isDevBuild() {
		return false
	}

	markerPath, err := pendingUpdatePath()
	if err != nil {
		return false
	}

	if _, err := os.Stat(markerPath); os.IsNotExist(err) {
		return false
	}

	data, err := os.ReadFile(markerPath)
	if err != nil {
		_ = os.Remove(markerPath)
		return false
	}

	var pending pendingUpdate
	if err := json.Unmarshal(data, &pending); err != nil {
		_ = os.Remove(markerPath)
		return false
	}

	// Already at or ahead of the pending version — e.g. the user ran
	// `aiscan update` in the meantime.
	currentV, err := semver.NewVersion(buildinfo.Version)
	if err != nil {
		_ = os.Remove(markerPath)
		return false
	}
	pendingV, err := semver.NewVersion(pending.Version)
	if err != nil {
		_ = os.Remove(markerPath)
		return false
	}
	if !currentV.LessThan(pendingV) {
		_ = os.Remove(markerPath)
		return false
	}

	execPath, err := os.Executable()
	if err != nil {
		_ = os.Remove(markerPath)
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()

	// go-selfupdate writes progress to raw fd 1; silence it (deferred so
	// stdout is restored even on panic).
	restoreStdout := suppressStdout()
	defer restoreStdout()
	err = selfupdate.UpdateTo(ctx, pending.AssetURL, pending.AssetName, execPath)

	// Always remove the marker — on failure the next background check will
	// detect the newer version again and write a fresh one.
	_ = os.Remove(markerPath)

	return err == nil
}

// ClearPendingUpdate removes the pending update marker file. Call after a
// successful manual update so a stale marker can't re-apply an old version.
func ClearPendingUpdate() {
	markerPath, err := pendingUpdatePath()
	if err != nil {
		return
	}
	_ = os.Remove(markerPath)
}

// CheckAndUpdateInBackground starts the daily update check in a goroutine and
// returns immediately. Errors are swallowed — updating must never disturb the
// command the user actually ran.
func CheckAndUpdateInBackground() {
	go func() {
		_, _ = checkAndUpdate(false)
	}()
}

// Check runs the throttled update check synchronously and reports whether a
// new binary was swapped into place. The daemon calls this on a ticker; a true
// result means the on-disk binary is newer than the running image, so the
// caller should restart at its next idle point to adopt it.
func Check() (updated bool, err error) {
	return checkAndUpdate(false)
}

// CheckNow is Check without the daily throttle — the user explicitly asked
// (the tray's "Check for updates"), so "I already checked today" is not an
// answer. The dev-build and kill-switch guards still hold and are reported as
// errors rather than silently skipped: an explicit ask deserves a reason.
func CheckNow() (updated bool, err error) {
	if Disabled() {
		return false, errors.New("autoupdater disabled (" + disableEnv + ")")
	}
	if isDevBuild() {
		return false, errors.New("dev build (" + buildinfo.Version + "); update from a release")
	}
	return checkAndUpdate(true)
}

// checkAndUpdate is phase 1: detect the latest release, write the marker,
// then attempt the swap inline. If the process exits mid-download the marker
// survives for ApplyPendingUpdate. Returns whether the swap completed. force
// skips the daily throttle (but never the dev-build or kill-switch guards).
func checkAndUpdate(force bool) (bool, error) {
	if Disabled() || isDevBuild() {
		return false, nil
	}

	if !force && !shouldCheck() {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()

	updater, err := NewUpdater()
	if err != nil {
		return false, err
	}

	release, found, err := updater.DetectLatest(ctx, Repository())
	if err != nil {
		// Leave the timestamp untouched so the next run retries.
		return false, err
	}
	if !found || release.LessOrEqual(buildinfo.Version) {
		_ = updateCheckTimestamp()
		return false, nil
	}

	// Marker first: the goroutine rarely outlives the user's command, and
	// the marker is what lets the next run finish the job.
	_ = writePendingUpdate(release)
	_ = updateCheckTimestamp()

	restoreStdout := suppressStdout()
	defer restoreStdout()

	if err := updater.UpdateTo(ctx, release, ""); err != nil {
		// Marker remains for phase 2 to retry.
		return false, err
	}

	ClearPendingUpdate()
	return true, nil
}

func writePendingUpdate(release *selfupdate.Release) error {
	markerPath, err := pendingUpdatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		return err
	}

	data, err := json.Marshal(pendingUpdate{
		Version:   release.Version(),
		AssetURL:  release.AssetURL,
		AssetName: release.AssetName,
	})
	if err != nil {
		return err
	}
	return os.WriteFile(markerPath, data, 0o644)
}

// shouldCheck reports whether the last check was more than checkInterval ago,
// tracked via the throttle file's mtime.
func shouldCheck() bool {
	dir, err := CacheDir()
	if err != nil {
		return true
	}
	info, err := os.Stat(filepath.Join(dir, updateCacheFile))
	if err != nil {
		return true
	}
	return time.Since(info.ModTime()) > checkInterval
}

func updateCheckTimestamp() error {
	dir, err := CacheDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, updateCacheFile), []byte(time.Now().Format(time.RFC3339)), 0o644)
}
