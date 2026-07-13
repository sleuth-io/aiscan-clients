# CLAUDE.md — aiscan-clients

Guidance for coding agents working in this repository.

## What this is

Open-source client apps for **aiscan**. A client captures local AI-tool usage on the user's
machine, **redacts** it, and **uploads** it to the aiscan server, which performs the analysis.
See `README.md` and `docs/transparency.md`.

## This repo is PUBLIC / open source — hard rules

- **No secrets** — no tokens, API keys, or private hostnames in code or docs.
- **No internal or customer names**, and no private repository names, anywhere in this repo.
- **Build only on public dependencies.** Reuse from other internal projects must be
  **copied/ported in**, never imported as a private module dependency.
- **Auth carries no embedded credentials** — device-code OAuth; the client holds only a
  per-user token.
- **Redaction is conservative** — when unsure, strip. Once content is uploaded, the on-device
  redaction is the only gate that ran before the wire.

## Architecture (don't redesign it)

- **Thin client:** capture → redact → upload. Parsing, normalization, analysis, and reporting
  are **all server-side**. Do not add analysis to the client.
- **Store-nothing** is a server guarantee; the client's job is to be *auditable* about what it
  sends and to surface that to the user (tray / popup).

## Layout

- `desktop/` — Go binary: CLI verbs + background agent + system tray.
- `extension/` — WebExtension (Chrome + Firefox), config-driven capture.
- `protocol/` — the **canonical** client↔server contract (JSON Schema). Clients and the server
  all validate/generate from these schemas; the shapes are not duplicated anywhere else. Change
  a shape here, once.
- `docs/` — public transparency docs.

## Releases — tag conventions

Multiple clients share this repo, so **no release uses a bare `vX.Y.Z` tag**. Each client
prefixes its tags with its directory name: `desktop-vX.Y.Z` (Go CLI, built by
`release-desktop.yml` on tag push) and `extension-vX.Y.Z` (browser extension, released by
`release-extension.yml` on manifest-version bumps). Never mark a release "latest" and never
link `releases/latest` — pin exact tags.

## desktop/ (Go) — commands & conventions

- Module `github.com/sleuth-io/aiscan-clients/desktop`, Go 1.24. Run commands from `desktop/`.
- Build `go build ./...` · Test `go test ./...` · Vet `go vet ./...`
- Keep capture/redact/upload/self-update **pure Go** (`CGO_ENABLED=0`). Only the system tray
  pulls in Cgo on macOS — isolate it so the rest cross-compiles cleanly.
- **Self-update restart:** as a long-running agent, the daemon must *restart itself* to adopt a
  new binary (swap on disk → restart at an idle point: exec-in-place, except the macOS daemon,
  which spawns a successor and exits — an exec'd process loses its LaunchServices registration
  and its tray icon never appears). The OS supervisor (launchd `KeepAlive`
  with `SuccessfulExit=false`) is crash recovery only — a clean Quit stays quit. There is no
  in-place hot reload — don't design for one.

## extension/ — conventions

- Manifest V3. **No remotely-hosted code** — capture rules come from server-fetched JSON config
  (data, not code). Prefer intercepting network responses over scraping the DOM.
- One codebase; per-browser differences live in the manifest only.

## Implementation specs

Detailed build specs are maintained privately; the operator provides the relevant client spec
when starting a build task. This file and the per-directory READMEs are the public guidance.
