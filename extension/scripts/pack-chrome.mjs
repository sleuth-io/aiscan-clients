// Pack dist/chrome/ into a signed CRX3 + its update manifest, for self-hosted enterprise
// force-install (Chrome ExtensionInstallForcelist policy).
//
//   npm run keygen:chrome   -> create the persistent signing key (run ONCE, then back it up)
//   npm run pack:chrome     -> build + emit dist/artifacts/aiscan-<version>.crx + update_manifest.xml
//
// The signing key fixes the extension ID forever. Lose it and every managed browser sees a
// different extension (new ID) and won't update — so it is gitignored and must be kept safe.

import { generateKeyPairSync } from 'node:crypto'
import { existsSync } from 'node:fs'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import crx3 from 'crx3'
import { runtimeFilesFromManifest, withTrailingSlash } from './lib.mjs'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const keyPath = process.env.CHROME_EXT_KEY || join(root, 'chrome-key.pem')

// --keygen: mint the signing key. Guarded so we never silently clobber an existing one.
if (process.argv.includes('--keygen')) {
  if (existsSync(keyPath)) {
    console.error(`refusing to overwrite existing key: ${keyPath}`)
    process.exit(1)
  }
  const { privateKey } = generateKeyPairSync('rsa', { modulusLength: 4096 })
  await writeFile(keyPath, privateKey.export({ type: 'pkcs8', format: 'pem' }))
  console.log(
    `wrote signing key → ${keyPath}\n` +
      'Back this up somewhere safe: it defines the extension ID and is required for every future release.',
  )
  process.exit(0)
}

if (!existsSync(keyPath)) {
  console.error(`no signing key at ${keyPath}\nrun: npm run keygen:chrome   (or set CHROME_EXT_KEY to an existing key)`)
  process.exit(1)
}

const srcDir = join(root, 'dist', 'chrome')
if (!existsSync(join(srcDir, 'manifest.json'))) {
  console.error('dist/chrome not built — run: npm run build:chrome')
  process.exit(1)
}

const manifest = JSON.parse(await readFile(join(srcDir, 'manifest.json'), 'utf8'))
const version = manifest.version

// Two URLs: the update manifest lives at a STABLE location (Pages) that IT bakes into policy
// once; the CRX itself is pinned to this version's Release — never releases/latest, which is
// meaningless in a multi-client repo (a desktop CLI release would hijack it and break auto-update).
const updateBase = withTrailingSlash(process.env.AISCAN_UPDATE_BASE_URL || 'https://sleuth-io.github.io/aiscan-clients/')
const releaseBase = withTrailingSlash(
  process.env.AISCAN_RELEASE_BASE_URL ||
    `https://github.com/sleuth-io/aiscan-clients/releases/download/extension-v${version}/`
)

const artifacts = join(root, 'dist', 'artifacts')
await mkdir(artifacts, { recursive: true })

// Stable filename; the per-version URL lives only in update_manifest.xml, regenerated each release.
const crxPath = join(artifacts, 'aiscan.crx')
const xmlPath = join(artifacts, 'update_manifest.xml')
const crxURL = `${releaseBase}aiscan.crx`

// Zip exactly the manifest + the scripts it declares (not whatever happens to sit in dist/chrome).
const files = ['manifest.json', ...runtimeFilesFromManifest(manifest)].map((f) => join(srcDir, f))

await crx3(files, { keyPath, crxPath, xmlPath, crxURL, appVersion: version })

// The extension ID is derived from the key; crx3 writes it into the update manifest as `appid`.
const xml = await readFile(xmlPath, 'utf8')
const appId = xml.match(/appid="([a-p]+)"/)?.[1] ?? '(see update_manifest.xml)'

// Guard against a wrong signing key: a mismatched key silently yields a NEW extension ID, which
// breaks auto-update for already-installed managed browsers. When the expected id is configured
// (release workflow), fail loudly instead.
const expectedId = process.env.AISCAN_EXPECTED_APP_ID
if (expectedId && expectedId !== appId) {
  console.error(`extension id mismatch: packed ${appId}, expected ${expectedId} — wrong signing key?`)
  process.exit(1)
}

console.log(
  `\nCRX packed (v${version}):\n` +
    `  crx   dist/artifacts/aiscan.crx        (publish as a Release asset)\n` +
    `  xml   dist/artifacts/update_manifest.xml  (publish to ${updateBase})\n` +
    `  id    ${appId}\n\n` +
    `IT force-install policy value (ExtensionInstallForcelist):\n` +
    `  ${appId};${updateBase}update_manifest.xml\n`,
)
