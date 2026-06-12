# aiscan desktop client

A single Go binary that runs as a background agent with a system-tray UI, and also exposes CLI
verbs. It captures local AI-tool usage, redacts it, uploads it to the aiscan server, and keeps
itself up to date.

## Responsibilities

- **Capture** local AI usage sources (read-only).
- **Redact** — a thin, stable pass that strips obvious secrets before upload.
- **Upload** the redacted capture to the server.
- **Tray UI** — status, last run, "Run now", "View last upload". The trust surface.
- **Self-update** — download signed, checksum-verified releases; swap the binary; restart.

It does **not** parse, normalize, analyze, or build reports — that is the server's job.

## Modes (one binary)

- `aiscan login` — authenticate (device-code OAuth).
- `aiscan run` — capture → redact → upload once.
- `aiscan daemon` — resident agent with the system tray (`--no-tray` for headless).

## Build notes

- Module: `github.com/sleuth-io/aiscan-clients/desktop` (Go 1.23).
- The capture/upload core is pure Go (`CGO_ENABLED=0`). Only the tray pulls in Cgo on macOS
  (Cocoa); it is pure-Go on Windows/Linux. Build the tray binary on a macOS runner.
- Self-update uses signed GitHub releases + atomic binary swap; as a long-running agent it must
  **restart itself** to adopt an update (exit at idle → relaunched by the OS supervisor:
  launchd `KeepAlive` on macOS).

## Layout

```
cmd/aiscan/      entry point
internal/        capture, redaction, upload, self-update, tray (to be built)
```
