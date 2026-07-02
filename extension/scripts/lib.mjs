// Pure packaging helpers, split out from the build/pack scripts so the branching logic
// (dev vs prod, per-browser manifest tweaks, the updates.json shape) is unit-testable
// without touching the filesystem. See scripts/lib.test.mjs.

// Host permissions that only make sense against a local Pulse dev instance. Stripped from prod
// builds so the shipped extension can't reach dev; kept in dev builds for local testing.
export const DEV_ONLY_HOST_PATTERNS = ['http://dev.pulse.sleuth.io/*', 'https://dev.pulse.sleuth.io/*']

export const withTrailingSlash = (url) => url.replace(/\/*$/, '/')

// The runtime scripts a manifest references (background + content scripts). Both the build (what to
// copy) and the Chrome pack (what to zip) derive their file list from this single source of truth,
// so adding or renaming a script can't silently ship a manifest that points at a missing file.
export function runtimeFilesFromManifest(manifest) {
  const files = new Set()
  const bg = manifest.background || {}
  if (bg.service_worker) files.add(bg.service_worker)
  for (const s of bg.scripts || []) files.add(s)
  for (const cs of manifest.content_scripts || []) for (const j of cs.js || []) files.add(j)
  return [...files]
}

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
  } else if (target === 'firefox') {
    // Firefox runs the background as scripts, not a service worker; drop the Chrome-only key so
    // AMO's linter doesn't warn (MANIFEST_FIELD_UNSUPPORTED). Guarded so we never leave Firefox
    // with no background entrypoint.
    if (m.background?.service_worker && m.background?.scripts?.length) delete m.background.service_worker
    // Firefox polls this updates.json (gecko.update_url) for self-hosted auto-update.
    if (isProd) m.browser_specific_settings.gecko.update_url = `${base}updates.json`
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
