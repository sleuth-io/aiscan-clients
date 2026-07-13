# aiscan desktop client

A single Go binary that runs as a background agent with a system-tray UI, and also exposes CLI
verbs. It captures local AI-tool usage, redacts it, uploads it to the aiscan server, and keeps
itself up to date.

## Responsibilities

- **Capture** local AI usage sources (read-only).
- **Redact** — a thin, stable pass that strips obvious secrets before upload.
- **Upload** the redacted capture to the server.
- **Tray UI** — status, last run, "Run now", "View last upload". The trust surface.
- **Self-update** — download checksum-verified releases; swap the binary; restart.

It does **not** parse, normalize, analyze, or build reports — that is the server's job.

## Modes (one binary)

- `aiscan login` — authenticate (device-code OAuth).
- `aiscan capture` — collect + redact + summarize locally; never uploads (read-only inspect).
- `aiscan sync` — capture → redact → upload the spans the server still needs (`--no-upload` to inspect).
- `aiscan update` — update to the latest release (`--check` to only look).
- `aiscan daemon` — resident agent with the system tray (`--no-tray` for headless).

All verbs accept `--instance URL`; the `AISCAN_INSTANCE` environment variable changes the
default (useful in the daemon's LaunchAgent plist to point one machine at a test server).

## The daemon and the tray

`aiscan daemon` syncs on an interval (default hourly, `--interval`) and renders its state in
the system tray — the trust surface: who is logged in, when the last sync ran, and direct
controls (**Sync now**, **Pause**, **Log in/out**, **Quit**). Scheduled syncs never prompt: no
cached token simply means "Log in to start syncing" in the menu. Logging in opens the browser
device-code approval; the username shown comes from the server's whoami endpoint, which doubles
as the token-validity probe.

One exception to "never prompt": the very first time the daemon runs on a machine with no
login, it opens the browser approval on its own. macOS hides menu-bar icons when the bar is
full, so on a fresh install the tray's "Log in" may not be visible at all — the one-shot prompt
lets the install start working anyway. A `login-prompted` stamp in the config dir guarantees it
never fires again, so logging out (or closing the browser tab) sticks.

The daemon logs to `~/Library/Logs/aiscan/daemon.log` on macOS (elsewhere:
`<user-cache>/aiscan/logs/daemon.log`) — the first place to look when a machine "isn't
syncing". A file lock guarantees a single instance; a second launch just says so and exits.

## macOS app (the pilot install)

`Aiscan.dmg` (a release asset on `desktop-v*` releases) wraps the same binary in a menu-bar-only
app bundle (`LSUIElement`). A double-click with no argv runs the daemon. Install flow:

1. Open the dmg, drag **Aiscan** to **Applications** (the drag is required: an app run in place
   from the dmg is App-Translocated to a read-only path — the daemon detects this and asks the
   user to move it rather than half-working).
2. Launch **Aiscan** from `/Applications`. The app is Developer ID signed, notarized, and
   stapled, so it opens on first double-click with no Gatekeeper prompt.
3. Success looks like: the aiscan bars icon appears in the menu bar and the browser opens the
   log-in approval page (first launch only; the icon can be hidden if the menu bar is full, but
   the login still works).

Releases are only signed/notarized when the Apple secrets are configured in CI (see
`packaging/macos/`); a build without them falls back to an ad-hoc signature, which on first
launch needs the one-time **System Settings → Privacy & Security → "Open Anyway"** step.

On first launch from `/Applications` the daemon installs a LaunchAgent
(`~/Library/LaunchAgents/io.sleuth.aiscan.plist`): `RunAtLoad` starts it at login and
`KeepAlive` with `SuccessfulExit=false` relaunches it only after a crash — a clean **Quit**
from the tray sticks until the next login or app launch.

**Uninstall:** Quit from the tray, `launchctl bootout gui/$UID/io.sleuth.aiscan` (or just
delete the plist), and trash `/Applications/Aiscan.app`.

## Self-update

Releases are GitHub Releases tagged `desktop-vX.Y.Z` (every client in this repo prefixes its
tags; the browser extension uses `extension-v*`), published by
`.github/workflows/release-desktop.yml`. The darwin binaries are built on a macOS runner (the
tray needs Cgo/Cocoa there) and the app bundle ships the identical binary the tarball carries,
so a self-update binary swap inside `Aiscan.app` never loses the tray. That tarball binary is
signed with the same Developer ID as the bundle, so the swap keeps the app launchable and
preserves its code identity (a binary the updater fetches over HTTP is not quarantined, so
Gatekeeper does not re-assess the bundle). The dmg is deliberately named without `<os>_<arch>`
tokens so go-selfupdate always picks the tarball.

On any run, at most once per day, the client checks for a newer release in the background and
swaps the binary in place — sha256-verified against the release's `checksums.txt`. Because the
process usually exits before the download finishes, a pending-update marker is written first and
the next run applies it, then re-execs so it already runs the new version (two-phase, ported
from the sx CLI).

The resident daemon re-runs the same throttled check on a ticker and restarts itself at the
next idle point to adopt the new binary — the OS supervisor is only for crash recovery, not
update adoption. The restart is an exec-in-place, except the macOS daemon, which spawns a
successor process and exits: an exec'd process keeps its stale LaunchServices registration and
its menu-bar icon silently never appears.

- `AISCAN_DISABLE_AUTOUPDATER=1` turns the background updater off; `aiscan update` still works.
- Dev builds never self-update: version `dev`, `-dirty`, or a git-describe suffix (`X.Y.Z-N-g<hash>`,
  a commit past the release tag — semver reads it as a *prerelease* of X.Y.Z, so updating would
  quietly replace a test build with the older official release).
- State lives in the user cache dir (`~/.cache/aiscan` on Linux, `~/Library/Caches/aiscan` on
  macOS): `last-update-check` (throttle) and `pending-update.json` (marker).

## Build notes

- Module: `github.com/sleuth-io/aiscan-clients/desktop` (Go 1.24).
- The capture/upload core is pure Go (`CGO_ENABLED=0`). Only the tray pulls in Cgo on macOS
  (Cocoa, via fyne.io/systray); it is pure-Go on Windows/Linux — which is why darwin release
  binaries are built on a macOS runner while linux/windows cross-compile from ubuntu.
- Self-update uses GitHub releases + atomic binary swap (see above). The long-running daemon
  adopts an update by **re-execing itself at an idle point**; launchd `KeepAlive`
  (`SuccessfulExit=false`) exists only to restart it after a crash.

## Layout

```
cmd/aiscan/       entry point
internal/         capture, redaction, upload, autoupdate, auth, cli (verbs + daemon agent), tray
packaging/macos/  Info.plist + AppIcon.png + entitlements.plist + make-dmg.sh (app bundle,
                  Developer ID signed + notarized, dmg)
```
