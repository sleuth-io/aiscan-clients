# Packaging & distribution

Two shipping paths:

- **Firefox** — a Mozilla-signed `.xpi`, self-hosted (unlisted), with self-hosted auto-update.
- **Chrome** — a self-signed `.crx` + update manifest, force-installed by the customer's IT via
  the `ExtensionInstallForcelist` policy (managed Chrome).

Releases are automated (see [CI/CD](#cicd) below); the local commands here are for testing the
pipeline or cutting a manual build.

## How it's hosted

The repo keeps **one** `manifest.json`. `scripts/build.mjs` stages it into `dist/<target>/` and
applies per-browser + per-environment tweaks; nothing is maintained twice. Build output lands in
`dist/` (gitignored). The runtime stays dependency-free — `web-ext` and `crx3` are devDependencies.

| File | Hosted on | Stable URL |
| --- | --- | --- |
| `update_manifest.xml` (Chrome) | GitHub Pages | `https://sleuth-io.github.io/aiscan-clients/update_manifest.xml` |
| `updates.json` (Firefox) | GitHub Pages | `https://sleuth-io.github.io/aiscan-clients/updates.json` |
| `aiscan.crx` | Release asset | `.../releases/download/extension-v<version>/aiscan.crx` |
| `aiscan.xpi` | Release asset | `.../releases/download/extension-v<version>/aiscan.xpi` |

The two Pages files are the **stable** URLs IT policy / Firefox reference once; their `version`
field and their download links are rewritten each release to pin that release's exact tag. So a
release only redeploys two tiny files to Pages.

The download links pin an exact tag rather than `releases/latest/download/` deliberately: several
clients share this repo, so "latest" is whatever shipped most recently — a desktop CLI release
would hijack it and break extension auto-update.

## Dev vs prod builds

`AISCAN_BUILD_ENV` controls environment-specific manifest tweaks:

- **dev** (default) — keeps the `dev.pulse.sleuth.io` host permissions and omits auto-update
  fields, so the build is loadable unpacked against your local Pulse instance.
- **prod** — strips the dev-only hosts and wires `update_url` (Chrome) / `gecko.update_url`
  (Firefox) to the Pages URLs. CI sets this for releases.

```
npm install              # once
npm run build            # dev build of both targets → dist/
AISCAN_BUILD_ENV=prod npm run build   # production build
```

## Versioning

The **release tag is the source of truth**: `extension-vX.Y.Z`. CI stamps that version into the
staged manifest at build time (the same way the desktop CLI stamps its version via ldflags), so
shipping is just pushing the tag — there is nothing to bump first. See [CI/CD](#cicd).

`manifest.json` and `package.json` therefore carry a `0.0.0` **placeholder**. That's deliberate: a
real number in the repo would be a second source of truth that could silently disagree with the tag
it shipped under. Local builds report `v0.0.0` unless you set `AISCAN_VERSION`.

Chrome constrains the shape — 1–4 dot-separated integers, each 0–65535, so no `-rc1` suffixes. The
build rejects an unusable `AISCAN_VERSION` up front rather than letting it fail deep in the CRX
pack, after signing. Both signers/updaters reject a re-upload of an existing version, so every tag
must be a new one.

## Firefox — signed XPI (local)

One-time: create an [addons.mozilla.org](https://addons.mozilla.org) account and API credentials
(Tools → Manage API Keys). Export them (never commit):

```
export WEB_EXT_API_KEY=user:12345:67
export WEB_EXT_API_SECRET=...
AISCAN_BUILD_ENV=prod npm run package:firefox
```

Produces `dist/artifacts/aiscan.xpi` (signed) and `dist/artifacts/updates.json`.

## Chrome — self-hosted CRX (local)

One-time: mint the signing key. It fixes the extension ID forever — **back it up**; it is
gitignored (`*.pem`).

```
npm run keygen:chrome        # writes chrome-key.pem
AISCAN_BUILD_ENV=prod npm run pack:chrome
```

Produces `dist/artifacts/aiscan.crx` and `dist/artifacts/update_manifest.xml`, and prints the
extension ID + the exact policy string IT needs:

```
<extension-id>;https://sleuth-io.github.io/aiscan-clients/update_manifest.xml
```

## CI/CD

- **[ci.yml](../.github/workflows/ci.yml)** runs on every PR and push: the `extension` job runs
  tests, `web-ext lint`, a build of both targets, and a production build + CRX pack (with a
  throwaway key) so an un-buildable change can't merge.
- **[release-extension.yml](../.github/workflows/release-extension.yml)** runs on an
  `extension-vX.Y.Z` tag. It stamps the tag's version into the build, signs + packs both browsers,
  publishes an `extension-v<version>` GitHub Release with the binaries, and deploys the Pages
  pointer files. Cutting a release is one step:

  ```
  git tag extension-v0.1.0 && git push origin extension-v0.1.0
  ```

  Tags are prefixed per client (the desktop CLI uses `desktop-v*`), so nothing owns a bare
  `vX.Y.Z`. If a release for the tag already exists the run no-ops rather than re-publishing.

### One-time repo setup

- **Settings → Pages → Source: GitHub Actions.**
- Repo secrets:
  - `CHROME_SIGNING_KEY` — base64 of your `keygen:chrome` key (`base64 -w0 chrome-key.pem`).
  - `AMO_JWT_ISSUER` / `AMO_JWT_SECRET` — addons.mozilla.org API credentials.
- Repo **variable** (optional but recommended): `CHROME_EXTENSION_ID` — the extension id printed by
  `pack:chrome`. When set, a release aborts if the signing key ever produces a different id (a wrong
  `CHROME_SIGNING_KEY` would otherwise silently ship a new id and break auto-update).

## Environment variables

| Var | Used by | Default |
| --- | --- | --- |
| `AISCAN_BUILD_ENV` | build | `dev` |
| `AISCAN_VERSION` | build | — (keeps the manifest's `0.0.0` placeholder; CI sets it from the tag) |
| `AISCAN_UPDATE_BASE_URL` | build + chrome pack | `https://sleuth-io.github.io/aiscan-clients/` |
| `AISCAN_RELEASE_BASE_URL` | chrome + firefox pack | `https://github.com/sleuth-io/aiscan-clients/releases/download/extension-v<version>/` |
| `CHROME_EXT_KEY` | chrome pack/keygen | `./chrome-key.pem` |
| `AISCAN_EXPECTED_APP_ID` | chrome pack | — (optional id guard) |
| `WEB_EXT_API_KEY` / `WEB_EXT_API_SECRET` | firefox sign | — (required) |
