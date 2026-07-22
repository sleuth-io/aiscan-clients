// Pure packaging helpers, split out from the build/pack scripts so the branching logic
// (dev vs prod, per-browser manifest tweaks, the updates.json shape) is unit-testable
// without touching the filesystem. See scripts/lib.test.mjs.

// Host permissions that only make sense against a local Pulse dev instance. Stripped from prod
// builds so the shipped extension can't reach dev; kept in dev builds for local testing.
export const DEV_ONLY_HOST_PATTERNS = ['http://dev.pulse.sleuth.io/*', 'https://dev.pulse.sleuth.io/*']

export const withTrailingSlash = (url) => url.replace(/\/*$/, '/')

// Whether a version string is one both stores will accept. Chrome is the strict one: 1-4
// dot-separated integers, each 0-65535, no leading zeros. Worth checking at the boundary because
// the release tag is the version's source of truth — a tag like `extension-v1.0.0-rc1` matches the
// workflow's `extension-v*` glob and would otherwise fail deep inside the CRX pack, after signing.
export function isPackableVersion(version) {
  const parts = String(version).split('.')
  if (parts.length > 4) return false
  return parts.every((p) => /^\d+$/.test(p) && !(p.length > 1 && p.startsWith('0')) && Number(p) <= 65535)
}

// The extension's tab page (opened via chrome.runtime.getURL, so referenced nowhere in the
// manifest). Listed here so the build stages it and the Chrome pack zips it — without this the
// page would silently be missing from dist and the toolbar icon would open a dead URL.
export const APP_PAGE_FILES = ['app.html', 'app.js', 'app.css']

// The runtime files a manifest references (background + content scripts) plus the app page files
// above. Both the build (what to copy) and the Chrome pack (what to zip) derive their file list
// from this single source of truth, so adding or renaming a file can't silently ship a manifest
// that points at a missing file.
export function runtimeFilesFromManifest(manifest) {
  const files = new Set()
  const bg = manifest.background || {}
  if (bg.service_worker) files.add(bg.service_worker)
  for (const s of bg.scripts || []) files.add(s)
  for (const cs of manifest.content_scripts || []) for (const j of cs.js || []) files.add(j)
  for (const f of APP_PAGE_FILES) files.add(f)
  return [...files]
}

// Apply the per-target, per-environment tweaks to a parsed manifest and return a new object
// (the input is left untouched). `updateBaseUrl` is where the stable pointer files are published.
// `version`, when given, replaces the manifest's placeholder — see the release tag note below.
export function transformManifest(manifest, { target, isProd, updateBaseUrl, version }) {
  if (!['firefox', 'chrome'].includes(target)) throw new Error(`unknown target: ${target}`)
  const m = structuredClone(manifest)
  const base = withTrailingSlash(updateBaseUrl)

  // The release tag is the source of truth for the version, mirroring the desktop CLI (which
  // stamps it via ldflags). The committed manifest carries a 0.0.0 placeholder, so CI passes the
  // tag's version here: the shipped manifest — the thing browsers compare to decide whether to
  // auto-update — then matches the tag it was released under by construction, with no bump to
  // forget. Dev builds pass nothing and stay on the placeholder.
  if (version) m.version = version

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
