#!/usr/bin/env bash
# Assemble Aiscan.app around a darwin aiscan binary, ad-hoc sign it, and wrap
# it in a dmg with an /Applications symlink so the install gesture (drag) is
# the interface. No Developer ID / notarization for the pilot: first launch
# needs the one-time System Settings → Privacy & Security → "Open Anyway"
# step, documented in desktop/README.md.
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

# Ad-hoc signature: no cert or Apple account, but Apple Silicon refuses
# unsigned arm64 binaries outright and a fresh seal covers the whole bundle.
codesign --force --deep --sign - "$APP"

ln -s /Applications "$STAGE/dmg/Applications"

mkdir -p "$OUT"
# No <os>_<arch> tokens in the name: go-selfupdate must keep matching the CLI
# tar.gz for binary swaps, never this dmg.
hdiutil create -volname "Aiscan" -srcfolder "$STAGE/dmg" -ov -format UDZO \
  "$OUT/Aiscan.dmg"
