// Pure packaging helpers, split out from the build/pack scripts so the branching logic
// (dev vs prod, per-browser manifest tweaks, the updates.json shape) is unit-testable
// without touching the filesystem. See scripts/lib.test.mjs.

// Host permissions that only make sense against a local Pulse dev instance. Stripped from prod
// builds so the shipped extension can't reach dev; kept in dev builds for local testing.
export const DEV_ONLY_HOST_PATTERNS = ['http://dev.pulse.sleuth.io/*', 'https://dev.pulse.sleuth.io/*']

const withTrailingSlash = (url) => url.replace(/\/*$/, '/')

// Apply the per-target, per-environment tweaks to a parsed manifest and return a new object
// (the input is left untouched). `updateBaseUrl` is where the stable pointer files are published.
export function transformManifest(manifest, { target, isProd, updateBaseUrl }) {
  if (!['firefox', 'chrome'].includes(target)) throw new Error(`unknown target: ${target}`)
  const m = structuredClone(manifest)
  const base = withTrailingSlash(updateBaseUrl)

  if (isProd) {
    m.host_permissions = (m.host_permissions || []).filter((h) => !DEV_ONLY_HOST_PATTERNS.includes(h))
  }

  if (target === 'chrome') {
    // gecko settings are Firefox-only; drop them so Chrome doesn't flag an unknown manifest key.
    delete m.browser_specific_settings
    // Self-hosted auto-update. The AMO linter rejects a top-level update_url, so Chrome-only.
    if (isProd) m.update_url = `${base}update_manifest.xml`
  } else if (target === 'firefox' && isProd) {
    // Firefox polls this updates.json (gecko.update_url) for self-hosted auto-update.
    m.browser_specific_settings.gecko.update_url = `${base}updates.json`
  }

  return m
}

// Build the Firefox self-hosted update manifest (updates.json) contents.
export function buildFirefoxUpdates({ addonId, version, releaseBase, hash }) {
  return {
    addons: {
      [addonId]: {
        updates: [{ version, update_link: `${withTrailingSlash(releaseBase)}aiscan.xpi`, update_hash: hash }],
      },
    },
  }
}
