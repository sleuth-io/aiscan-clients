# aiscan browser extension

Captures AI web usage (ChatGPT, Claude.ai, Gemini) and uploads a redacted capture to the aiscan
server. One codebase builds for Chrome and Firefox (Manifest V3).

## Design

- **Thin capture, server does the rest.** The extension captures conversation data, redacts it,
  and uploads it; parsing and analysis happen server-side.
- **Config-driven capture.** What to capture (which request URLs, which response fields) is
  driven by **server-fetched JSON config**, not hard-coded — so capture can adapt to site
  changes without shipping a new extension version. Prefer intercepting network responses over
  scraping the DOM (more stable).
- **Manifest V3 reality.** No remotely-hosted code (config/data only). Background service
  workers are short-lived, so capture is continuous/passive rather than holding a connection.
- **Trust surface.** The on-page panel shows status and what was last uploaded, and stores
  config + the auth token in `chrome.storage`.

## Layout (to be built)

```
src/            shared capture engine + config interpreter
manifest/       per-browser manifest differences (chrome, firefox)
```

## Packaging & distribution

- [DISTRIBUTION.md](DISTRIBUTION.md) — the map: how the whole build → sign → release → install flow
  fits together, where everything lives, and how it's hosted. Start here if you're new to it.
- [PACKAGING.md](PACKAGING.md) — the manual: exact commands and env vars for building the signed
  Firefox `.xpi` and the Chrome `.crx` + update manifest.
