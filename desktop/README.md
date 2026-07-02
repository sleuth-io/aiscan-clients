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

## Self-update

Releases are GitHub Releases tagged `desktop-vX.Y.Z` (every client in this repo prefixes its
tags; the browser extension uses `extension-v*`), published by
`.github/workflows/release-desktop.yml`.

On any run, at most once per day, the client checks for a newer release in the background and
swaps the binary in place — sha256-verified against the release's `checksums.txt`. Because the
process usually exits before the download finishes, a pending-update marker is written first and
the next run applies it, then re-execs so it already runs the new version (two-phase, ported
from the sx CLI).

- `AISCAN_DISABLE_AUTOUPDATER=1` turns the background updater off; `aiscan update` still works.
- Dev builds (version `dev` or `-dirty`) never self-update.
- State lives in the user cache dir (`~/.cache/aiscan` on Linux, `~/Library/Caches/aiscan` on
  macOS): `last-update-check` (throttle) and `pending-update.json` (marker).

## Build notes

- Module: `github.com/sleuth-io/aiscan-clients/desktop` (Go 1.24).
- The capture/upload core is pure Go (`CGO_ENABLED=0`). Only the tray pulls in Cgo on macOS
  (Cocoa); it is pure-Go on Windows/Linux. Build the tray binary on a macOS runner.
- Self-update uses GitHub releases + atomic binary swap (see above). When the daemon lands, as a
  long-running agent it must **restart itself** to adopt an update (exit at idle → relaunched by
  the OS supervisor: launchd `KeepAlive` on macOS).

## Layout

```
cmd/aiscan/      entry point
internal/        capture, redaction, upload, autoupdate; tray (to be built)
```
