#!/usr/bin/env bash
# Assemble Aiscan.app around a darwin aiscan binary, sign it, and wrap it in a
# dmg with an /Applications symlink so the install gesture (drag) is the
# interface.
#
# Signing is env-driven so the same script serves local dev and CI:
#
#   MACOS_SIGN_IDENTITY   Developer ID Application identity, e.g.
#                         "Developer ID Application: Acme (TEAMID)". Unset →
#                         ad-hoc ("-"): the app still runs on Apple Silicon
#                         (arm64 refuses truly unsigned code) but is not
#                         Gatekeeper-clean. `make dmg` on a dev machine uses
#                         this path.
#   MACOS_ENTITLEMENTS    Optional entitlements plist embedded under the
#                         hardened runtime. A pure-Go + Cocoa-tray app needs
#                         none; the file exists so the tarball binary and the
#                         bundled binary carry identical entitlements.
#   MACOS_NOTARY_KEY / MACOS_NOTARY_KEY_ID / MACOS_NOTARY_ISSUER
#                         App Store Connect API key (p8 path + key id + issuer
#                         uuid). When all three are set *and* a real signing
#                         identity is used, the app and dmg are notarized and
#                         stapled, so first launch of a browser-downloaded dmg
#                         has no Gatekeeper prompt — online or offline.
#
# Usage: make-dmg.sh <aiscan-binary> <version> <output-dir>
set -euo pipefail

BIN="$1"
VERSION="$2"
OUT="$3"
HERE="$(cd "$(dirname "$0")" && pwd)"

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

APP="$STAGE/dmg/Aiscan.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
sed "s/__VERSION__/${VERSION}/g" "$HERE/Info.plist" > "$APP/Contents/Info.plist"
install -m 0755 "$BIN" "$APP/Contents/MacOS/aiscan"

# Build AppIcon.icns from the committed 1024px source. sips/iconutil are macOS
# only, which is fine: the dmg is only ever assembled on the macOS runner.
ICONSET="$STAGE/AppIcon.iconset"
mkdir -p "$ICONSET"
for size in 16 32 128 256 512; do
  sips -z "$size" "$size" "$HERE/AppIcon.png" \
    --out "$ICONSET/icon_${size}x${size}.png" >/dev/null
  sips -z $((size * 2)) $((size * 2)) "$HERE/AppIcon.png" \
    --out "$ICONSET/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/AppIcon.icns"

# Sign the bundle. With a Developer ID identity we enable the hardened runtime
# (--options runtime) and a secure timestamp, both required for notarization;
# signing the .app seals its resources and (re)signs the main executable, so no
# separate inside-out pass is needed for a bundle with no nested code. Without
# an identity we fall back to an ad-hoc seal — arm64 refuses unsigned binaries
# outright, and a fresh seal still covers the whole bundle.
IDENTITY="${MACOS_SIGN_IDENTITY:-}"
sign_args=(--force)
if [ -n "$IDENTITY" ]; then
  sign_args+=(--options runtime --timestamp)
  [ -n "${MACOS_ENTITLEMENTS:-}" ] && sign_args+=(--entitlements "$MACOS_ENTITLEMENTS")
  sign_args+=(--sign "$IDENTITY")
else
  sign_args+=(--sign -)
fi
codesign "${sign_args[@]}" "$APP"

# Notarize + staple the .app before packaging, so the app dragged out of the
# dmg carries its own ticket and verifies offline. Only runs with a real
# identity and full notary credentials; skipped for ad-hoc/local builds.
can_notarize=0
if [ -n "$IDENTITY" ] && [ -n "${MACOS_NOTARY_KEY:-}" ] \
   && [ -n "${MACOS_NOTARY_KEY_ID:-}" ] && [ -n "${MACOS_NOTARY_ISSUER:-}" ]; then
  can_notarize=1
fi

notarize() { # $1 = path to a notarizable container (.zip/.app/.dmg)
  # --timeout bounds the wait: notarytool usually returns in minutes. 2h is a
  # generous ceiling for a genuinely backlogged notary service — slow enough to
  # ride out a bad day, but bounded so the job can't hang indefinitely.
  xcrun notarytool submit "$1" \
    --key "$MACOS_NOTARY_KEY" \
    --key-id "$MACOS_NOTARY_KEY_ID" \
    --issuer "$MACOS_NOTARY_ISSUER" \
    --wait --timeout 2h
}

if [ "$can_notarize" = 1 ]; then
  # notarytool needs a container; ditto (not zip) preserves the signature.
  APPZIP="$STAGE/Aiscan.app.zip"
  ditto -c -k --keepParent "$APP" "$APPZIP"
  notarize "$APPZIP"
  xcrun stapler staple "$APP"
fi

ln -s /Applications "$STAGE/dmg/Applications"

mkdir -p "$OUT"
# No <os>_<arch> tokens in the name: go-selfupdate must keep matching the CLI
# tar.gz for binary swaps, never this dmg.
hdiutil create -volname "Aiscan" -srcfolder "$STAGE/dmg" -ov -format UDZO \
  "$OUT/Aiscan.dmg"

# Sign, notarize, and staple the dmg itself so the download the user
# double-clicks is Gatekeeper-clean.
if [ -n "$IDENTITY" ]; then
  codesign --force --timestamp --sign "$IDENTITY" "$OUT/Aiscan.dmg"
fi
if [ "$can_notarize" = 1 ]; then
  notarize "$OUT/Aiscan.dmg"
  xcrun stapler staple "$OUT/Aiscan.dmg"
fi
