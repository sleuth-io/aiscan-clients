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
mkdir -p "$APP/Contents/MacOS"
sed "s/__VERSION__/${VERSION}/g" "$HERE/Info.plist" > "$APP/Contents/Info.plist"
install -m 0755 "$BIN" "$APP/Contents/MacOS/aiscan"

# Ad-hoc signature: no cert or Apple account, but Apple Silicon refuses
# unsigned arm64 binaries outright and a fresh seal covers the whole bundle.
codesign --force --deep --sign - "$APP"

ln -s /Applications "$STAGE/dmg/Applications"

mkdir -p "$OUT"
# No <os>_<arch> tokens in the name: go-selfupdate must keep matching the CLI
# tar.gz for binary swaps, never this dmg.
hdiutil create -volname "Aiscan" -srcfolder "$STAGE/dmg" -ov -format UDZO \
  "$OUT/Aiscan.dmg"
