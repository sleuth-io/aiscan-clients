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
| `aiscan.crx` | Release asset | `.../releases/latest/download/aiscan.crx` |
| `aiscan.xpi` | Release asset | `.../releases/latest/download/aiscan.xpi` |

The two Pages files are the **stable** URLs IT policy / Firefox reference once; their `version`
field bumps each release and their download links point at the stable `releases/latest/download/`
URLs. So a release only redeploys two tiny files to Pages.

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

Bump `version` in [manifest.json](manifest.json) for every release — the release workflow keys off
it, and both signers/updaters reject a re-upload of the same version. `package.json` mirrors it;
`manifest.json` is the source of truth.

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
- **[release-extension.yml](../.github/workflows/release-extension.yml)** runs on push to `main`.
  If `manifest.json`'s version hasn't been released yet, it signs + packs both browsers, publishes
  a `v<version>` GitHub Release with the binaries, and deploys the Pages pointer files. No version
  bump ⇒ no-op.

### One-time repo setup

- **Settings → Pages → Source: GitHub Actions.**
- Repo secrets:
  - `CHROME_SIGNING_KEY` — base64 of your `keygen:chrome` key (`base64 -w0 chrome-key.pem`).
  - `AMO_JWT_ISSUER` / `AMO_JWT_SECRET` — addons.mozilla.org API credentials.

## Environment variables

| Var | Used by | Default |
| --- | --- | --- |
| `AISCAN_BUILD_ENV` | build | `dev` |
| `AISCAN_UPDATE_BASE_URL` | build + chrome pack | `https://sleuth-io.github.io/aiscan-clients/` |
| `AISCAN_RELEASE_BASE_URL` | chrome + firefox pack | `https://github.com/sleuth-io/aiscan-clients/releases/latest/download/` |
| `CHROME_EXT_KEY` | chrome pack/keygen | `./chrome-key.pem` |
| `WEB_EXT_API_KEY` / `WEB_EXT_API_SECRET` | firefox sign | — (required) |
