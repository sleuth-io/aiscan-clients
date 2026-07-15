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
//
// Version (AISCAN_VERSION): stamped into the staged manifest. The release workflow derives it from
// the extension-vX.Y.Z tag; unset (local builds) keeps the manifest's 0.0.0 placeholder.

import { access, copyFile, mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { isPackableVersion, runtimeFilesFromManifest, transformManifest } from './lib.mjs'

const target = process.argv[2]
if (!['firefox', 'chrome'].includes(target)) {
  console.error('usage: node scripts/build.mjs <firefox|chrome>')
  process.exit(1)
}

const isProd = process.env.AISCAN_BUILD_ENV === 'prod'

const version = process.env.AISCAN_VERSION
if (version && !isPackableVersion(version)) {
  console.error(
    `AISCAN_VERSION="${version}" is not a usable extension version.\n` +
      'Chrome requires 1-4 dot-separated integers, each 0-65535 (e.g. 1.2.3) — no pre-release suffixes.',
  )
  process.exit(1)
}

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const outDir = join(root, 'dist', target)

// Where the stable pointer files (Chrome update manifest / Firefox updates.json) are published.
const updateBaseUrl = process.env.AISCAN_UPDATE_BASE_URL || 'https://sleuth-io.github.io/aiscan-clients/'

await rm(outDir, { recursive: true, force: true })
await mkdir(outDir, { recursive: true })

const source = JSON.parse(await readFile(join(root, 'manifest.json'), 'utf8'))
const manifest = transformManifest(source, { target, isProd, updateBaseUrl, version })

// Stage exactly the scripts the manifest declares — fail loudly if one is missing rather than
// shipping an extension whose manifest points at an absent file.
const runtimeFiles = runtimeFilesFromManifest(source)
for (const f of runtimeFiles) {
  await access(join(root, f)).catch(() => {
    console.error(`manifest references "${f}" but it does not exist`)
    process.exit(1)
  })
}

await writeFile(join(outDir, 'manifest.json'), JSON.stringify(manifest, null, 2) + '\n')
for (const f of runtimeFiles) await copyFile(join(root, f), join(outDir, f))

console.log(`built ${target} (${isProd ? 'prod' : 'dev'}) → dist/${target}/ (v${manifest.version})`)
