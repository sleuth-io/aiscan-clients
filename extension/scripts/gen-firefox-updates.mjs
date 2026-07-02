// After `web-ext sign` produces a Mozilla-signed XPI, normalize the release artifacts:
//   1. copy the signed .xpi to dist/artifacts/aiscan.xpi (stable filename for the Release asset)
//   2. write dist/artifacts/updates.json — the self-hosted update manifest Firefox polls
//      (referenced by manifest gecko.update_url in prod builds).
//
// The add-on id must match manifest.browser_specific_settings.gecko.id.

import { createHash } from 'node:crypto'
import { readdirSync } from 'node:fs'
import { copyFile, readFile, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { buildFirefoxUpdates } from './lib.mjs'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const artifacts = join(root, 'dist', 'artifacts')

const manifest = JSON.parse(await readFile(join(root, 'manifest.json'), 'utf8'))
const version = manifest.version
const addonId = manifest.browser_specific_settings?.gecko?.id
if (!addonId) {
  console.error('manifest is missing browser_specific_settings.gecko.id')
  process.exit(1)
}

const releaseBase = (process.env.AISCAN_RELEASE_BASE_URL || 'https://github.com/sleuth-io/aiscan-clients/releases/latest/download/').replace(/\/*$/, '/')

// web-ext writes the signed file into dist/artifacts as <name>-<version>.xpi; grab the newest xpi
// that isn't our own normalized output.
const signed = readdirSync(artifacts).find((f) => f.endsWith('.xpi') && f !== 'aiscan.xpi')
if (!signed) {
  console.error(`no signed .xpi found in ${artifacts} — run web-ext sign first`)
  process.exit(1)
}

const xpiPath = join(artifacts, 'aiscan.xpi')
await copyFile(join(artifacts, signed), xpiPath)

const hash = 'sha256:' + createHash('sha256').update(await readFile(xpiPath)).digest('hex')

const updates = buildFirefoxUpdates({ addonId, version, releaseBase, hash })
await writeFile(join(artifacts, 'updates.json'), JSON.stringify(updates, null, 2) + '\n')

console.log(
  `\nFirefox packaged (v${version}):\n` +
    `  xpi     dist/artifacts/aiscan.xpi          (publish as a Release asset)\n` +
    `  updates dist/artifacts/updates.json        (publish to Pages)\n` +
    `  id      ${addonId}\n`,
)
