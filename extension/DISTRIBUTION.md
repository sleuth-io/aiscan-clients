# Sleuth AI Insights — how packaging & distribution works

A technical orientation for anyone picking this up cold. It explains **what the pieces are, where
they live, and how a build gets from source to an installed browser extension**. For the exact
commands and env vars, see [PACKAGING.md](PACKAGING.md); this doc is the map, that one is the manual.

## TL;DR

The browser extension lives in [`/extension`](.). One codebase builds for both **Chrome** and
**Firefox** from a single `manifest.json`. On an `extension-vX.Y.Z` tag, GitHub Actions signs and
packages both, publishes a **GitHub Release** with the binaries, and deploys two small "where's the
current version" pointer files to **GitHub Pages**. Chrome is installed by the customer's IT via
enterprise policy; Firefox is installed from a link. Both auto-update.

## Where things live

| Path | What |
| --- | --- |
| `extension/manifest.json` | The single source of truth for name, permissions, IDs. Its `version` is a `0.0.0` placeholder — the release tag supplies the real one. |
| `extension/background.js`, `content.js` | The runtime (capture + upload). |
| `extension/scripts/build.mjs` | Stages the manifest + declared scripts into `dist/<target>/`. |
| `extension/scripts/pack-chrome.mjs` | Signs the Chrome `.crx` and writes `update_manifest.xml`. |
| `extension/scripts/gen-firefox-updates.mjs` | Writes Firefox `updates.json` after signing. |
| `extension/scripts/lib.mjs` (+ `lib.test.mjs`) | Pure helpers, unit-tested. |
| `extension/pages/index.html` | The public landing page served on GitHub Pages. |
| `.github/workflows/ci.yml` | Tests + build check on every PR. |
| `.github/workflows/release-extension.yml` | The release pipeline (runs on `extension-v*` tags). |
| `extension/dist/` | Build output. Gitignored — never committed. |

## How a build works

Everything runs through npm scripts (mirrored as `make` targets). One manifest is staged per
browser, with small tweaks applied by environment:

- **`AISCAN_BUILD_ENV=dev`** (default) — keeps the local dev host permission and omits
  auto-update wiring, so it's loadable unpacked for local testing.
- **`AISCAN_BUILD_ENV=prod`** — strips the dev host and wires auto-update. Used by CI.

Then:

- **Firefox** → `web-ext sign` uploads to Mozilla, which returns a **signed `.xpi`**. We also emit
  `updates.json` (the self-hosted update feed).
- **Chrome** → `crx3` produces a signed **`.crx`** plus `update_manifest.xml` (the update feed).

The runtime ships **zero dependencies**; `web-ext` and `crx3` are devDependencies used only for
packaging.

## CI/CD

- **`ci.yml`** — on every PR and push: runs tests, `web-ext lint`, builds both targets, and does a
  production build + CRX pack with a throwaway key. This keeps an un-buildable change from merging.
- **`release-extension.yml`** — on an `extension-vX.Y.Z` tag: stamps the tag's version into the
  build, signs + packs both browsers, creates an `extension-v<version>` GitHub Release with the
  binaries attached, and deploys the pointer files to Pages. So cutting a release is just:
  `git tag extension-v0.1.0 && git push origin extension-v0.1.0`.

  The tag is the **only** place the version is declared — `manifest.json` ships a `0.0.0`
  placeholder that the build overwrites. That mirrors the desktop CLI (which stamps its version via
  ldflags) and means the shipped manifest can't disagree with the tag it was released under.

## Hosting map

Two kinds of URL. The **pointer files** are stable (referenced once by IT policy / Firefox); the
**binaries** are Release assets at a URL pinned to that release's exact tag.

| File | Hosted on | URL |
| --- | --- | --- |
| `update_manifest.xml` (Chrome feed) | GitHub Pages | `https://sleuth-io.github.io/aiscan-clients/update_manifest.xml` |
| `updates.json` (Firefox feed) | GitHub Pages | `https://sleuth-io.github.io/aiscan-clients/updates.json` |
| `aiscan.crx` | Release asset | `.../releases/download/extension-v<version>/aiscan.crx` |
| `aiscan.xpi` | Release asset | `.../releases/download/extension-v<version>/aiscan.xpi` |

Each release only redeploys the two tiny pointer files to Pages; their `version` field and their
download links are rewritten to pin the new tag. Nothing points at `releases/latest/download` —
several clients share this repo, so "latest" is whatever shipped most recently, and a desktop CLI
release would hijack it and break extension auto-update.

## How users install

- **Chrome / Edge (managed):** the customer's IT adds an `ExtensionInstallForcelist` policy entry:
  `<chrome-extension-id>;https://sleuth-io.github.io/aiscan-clients/update_manifest.xml`.
  Chrome silently installs and auto-updates. (Off-store `.crx` files can't be installed on
  unmanaged Chrome — enterprise policy is the supported path.)
- **Firefox:** users click the install link on the [Pages site](https://sleuth-io.github.io/aiscan-clients/),
  which the release rewrites to point at that release's exact `.xpi`. Firefox installs with one
  permission prompt and auto-updates via `updates.json`.

## Identities

Both are permanent once shipped and are **public by design** (they appear in the update feeds):

- **Chrome extension ID:** `jclbfngmnnominefbkdlcldbdkabknjj` (derived from the signing key).
- **Firefox add-on ID:** `ai-insights@sleuth.io` (set in `manifest.json`).

## Signing credentials & secrets

The signing material is **never** in this repo. It's held externally and injected into CI via
GitHub Actions secrets:

| GitHub secret / variable | What it is | Where the source lives |
| --- | --- | --- |
| `CHROME_SIGNING_KEY` | base64 of `chrome-key.pem`, the RSA key that defines the Chrome extension ID | the team password manager (1Password) — see [operations note](#operations-note) |
| `AMO_JWT_ISSUER` / `AMO_JWT_SECRET` | addons.mozilla.org API credentials used by `web-ext sign` | a Sleuth-owned AMO account — see [operations note](#operations-note) |
| `CHROME_EXTENSION_ID` (variable, optional) | the expected Chrome ID; if set, a release aborts when the key would produce a different ID | n/a (just the ID above) |

Losing `chrome-key.pem` is the one unrecoverable event: a different key means a different extension
ID, which breaks auto-update for every already-installed managed browser. That's why it's backed up
in 1Password and why the optional `CHROME_EXTENSION_ID` guard exists.

### One-time GitHub setup

- **Settings → Pages → Source: GitHub Actions** (required, or the Pages deploy fails).
- The three secrets + optional variable above.

<a name="operations-note"></a>
## Operations note

The exact owners/locations of the signing credentials (which AMO account, which 1Password item) are
recorded internally, not in this public repo. Ask the maintainers or check the team ops notes.
