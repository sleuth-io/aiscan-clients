// Stage the extension's runtime files into dist/<target>/ for packaging.
//
// The repo keeps ONE manifest.json (source of truth). This script copies the runtime
// files into a per-target directory and applies the few browser- and environment-specific
// manifest tweaks so we never hand-maintain multiple manifests.
//
//   node scripts/build.mjs firefox   -> dist/firefox/  (fed to `web-ext sign`)
//   node scripts/build.mjs chrome    -> dist/chrome/   (fed to scripts/pack-chrome.mjs)
//
// Environment (AISCAN_BUILD_ENV):
//   dev  (default) — keeps the local dev host, omits auto-update fields; loadable unpacked.
//   prod           — strips dev-only hosts and wires self-hosted auto-update (used by CI release).

import { copyFile, mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const target = process.argv[2]
if (!['firefox', 'chrome'].includes(target)) {
  console.error('usage: node scripts/build.mjs <firefox|chrome>')
  process.exit(1)
}

const isProd = process.env.AISCAN_BUILD_ENV === 'prod'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const outDir = join(root, 'dist', target)

// The whole runtime is manifest + these two scripts (see SPIKE.md). Everything else in the
// directory is tests, packaging, or docs and must NOT ship in the extension.
const RUNTIME_FILES = ['background.js', 'content.js']

// Host permissions that only make sense against a local Pulse dev instance. Stripped from prod
// builds so the shipped extension can't reach dev; kept in dev builds for local testing.
const DEV_ONLY_HOST_PATTERNS = ['http://dev.pulse.sleuth.io/*', 'https://dev.pulse.sleuth.io/*']

// Where the stable pointer files (Chrome update manifest / Firefox updates.json) are published.
const UPDATE_BASE_URL = (process.env.AISCAN_UPDATE_BASE_URL || 'https://sleuth-io.github.io/aiscan-clients/').replace(/\/*$/, '/')

await rm(outDir, { recursive: true, force: true })
await mkdir(outDir, { recursive: true })

const manifest = JSON.parse(await readFile(join(root, 'manifest.json'), 'utf8'))

if (isProd) {
  manifest.host_permissions = (manifest.host_permissions || []).filter((h) => !DEV_ONLY_HOST_PATTERNS.includes(h))
}

if (target === 'chrome') {
  // gecko settings are Firefox-only; drop them so Chrome doesn't flag an unknown manifest key.
  delete manifest.browser_specific_settings
  if (isProd) {
    // Self-hosted auto-update: Chrome polls this update manifest (only meaningful once installed
    // via enterprise policy). The AMO linter rejects a top-level update_url, so Chrome-only.
    manifest.update_url = `${UPDATE_BASE_URL}update_manifest.xml`
  }
} else if (target === 'firefox' && isProd) {
  // Self-hosted auto-update for Firefox: gecko.update_url points at the updates.json we publish.
  manifest.browser_specific_settings.gecko.update_url = `${UPDATE_BASE_URL}updates.json`
}

await writeFile(join(outDir, 'manifest.json'), JSON.stringify(manifest, null, 2) + '\n')
for (const f of RUNTIME_FILES) await copyFile(join(root, f), join(outDir, f))

console.log(`built ${target} (${isProd ? 'prod' : 'dev'}) → dist/${target}/ (v${manifest.version})`)
