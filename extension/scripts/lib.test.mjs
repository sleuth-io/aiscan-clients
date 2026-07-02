import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { buildFirefoxUpdates, DEV_ONLY_HOST_PATTERNS, transformManifest } from './lib.mjs'

const FIXTURE = {
  name: 'Test',
  version: '1.2.3',
  host_permissions: [
    'https://claude.ai/*',
    'http://dev.pulse.sleuth.io/*',
    'https://dev.pulse.sleuth.io/*',
    'https://app.skills.new/*',
  ],
  browser_specific_settings: { gecko: { id: 'x@y', strict_min_version: '115.0' } },
}
const BASE = 'https://example.test/base/'

test('dev build keeps dev hosts and adds no auto-update fields', () => {
  const fx = transformManifest(FIXTURE, { target: 'firefox', isProd: false, updateBaseUrl: BASE })
  assert.deepEqual(fx.host_permissions, FIXTURE.host_permissions)
  assert.equal(fx.browser_specific_settings.gecko.update_url, undefined)

  const ch = transformManifest(FIXTURE, { target: 'chrome', isProd: false, updateBaseUrl: BASE })
  assert.ok(ch.host_permissions.includes('http://dev.pulse.sleuth.io/*'))
  assert.equal(ch.update_url, undefined)
})

test('prod build strips only the dev-only hosts', () => {
  const ch = transformManifest(FIXTURE, { target: 'chrome', isProd: true, updateBaseUrl: BASE })
  for (const h of DEV_ONLY_HOST_PATTERNS) assert.ok(!ch.host_permissions.includes(h))
  assert.ok(ch.host_permissions.includes('https://claude.ai/*'))
  assert.ok(ch.host_permissions.includes('https://app.skills.new/*'))
})

test('prod chrome drops gecko settings and wires update_url', () => {
  const ch = transformManifest(FIXTURE, { target: 'chrome', isProd: true, updateBaseUrl: BASE })
  assert.equal(ch.browser_specific_settings, undefined)
  assert.equal(ch.update_url, 'https://example.test/base/update_manifest.xml')
})

test('prod firefox keeps gecko and wires gecko.update_url (no top-level update_url)', () => {
  const fx = transformManifest(FIXTURE, { target: 'firefox', isProd: true, updateBaseUrl: BASE })
  assert.equal(fx.update_url, undefined)
  assert.equal(fx.browser_specific_settings.gecko.update_url, 'https://example.test/base/updates.json')
  assert.equal(fx.browser_specific_settings.gecko.id, 'x@y')
})

test('update base url is normalized to a single trailing slash', () => {
  const ch = transformManifest(FIXTURE, { target: 'chrome', isProd: true, updateBaseUrl: 'https://example.test/base' })
  assert.equal(ch.update_url, 'https://example.test/base/update_manifest.xml')
})

test('transformManifest does not mutate its input', () => {
  const before = JSON.stringify(FIXTURE)
  transformManifest(FIXTURE, { target: 'chrome', isProd: true, updateBaseUrl: BASE })
  assert.equal(JSON.stringify(FIXTURE), before)
})

test('transformManifest rejects an unknown target', () => {
  assert.throws(() => transformManifest(FIXTURE, { target: 'safari', isProd: false, updateBaseUrl: BASE }))
})

test('buildFirefoxUpdates produces the AMO self-hosted shape keyed by add-on id', () => {
  const u = buildFirefoxUpdates({
    addonId: 'a@b',
    version: '1.2.3',
    releaseBase: 'https://r.test/dl/',
    hash: 'sha256:deadbeef',
  })
  assert.deepEqual(u, {
    addons: {
      'a@b': {
        updates: [{ version: '1.2.3', update_link: 'https://r.test/dl/aiscan.xpi', update_hash: 'sha256:deadbeef' }],
      },
    },
  })
})

test('buildFirefoxUpdates normalizes the release base trailing slash', () => {
  const u = buildFirefoxUpdates({ addonId: 'a@b', version: '1', releaseBase: 'https://r.test/dl', hash: 'h' })
  assert.equal(u.addons['a@b'].updates[0].update_link, 'https://r.test/dl/aiscan.xpi')
})

// The real manifest must carry what packaging relies on, or a release would silently misbuild.
test('manifest.json carries the invariants packaging depends on', () => {
  const root = dirname(dirname(fileURLToPath(import.meta.url)))
  const m = JSON.parse(readFileSync(join(root, 'manifest.json'), 'utf8'))
  assert.ok(m.browser_specific_settings?.gecko?.id, 'gecko.id must be set for Firefox signing')
  // Prod stripping is only meaningful if the dev hosts are present in source.
  for (const h of DEV_ONLY_HOST_PATTERNS) assert.ok(m.host_permissions.includes(h), `${h} present in source`)
})
