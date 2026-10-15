#!/bin/bash
# Build FioTransfer.app and package it as a .dmg and .zip in dist/.
#
#   scripts/package.sh [version]
#
# Optional signing/notarization (all must be set to notarize):
#   MACOS_SIGN_IDENTITY   "Developer ID Application: Name (TEAMID)"; ad-hoc signed if unset
#   APPLE_ID, APPLE_TEAM_ID, APPLE_APP_PASSWORD   for notarytool
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-dev}"
PLATFORM="${PLATFORM:-darwin/universal}"
WAILS="$(command -v wails || echo "$HOME/go/bin/wails")"
APP="build/bin/FioTransfer.app"
NAME="FioTransfer-${VERSION}"

"$WAILS" build -platform "$PLATFORM" -clean -trimpath -ldflags "-s -w -X main.version=${VERSION}"

PLIST="$APP/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString ${VERSION#v}" "$PLIST"
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion ${VERSION#v}" "$PLIST"

if [[ -n "${MACOS_SIGN_IDENTITY:-}" ]]; then
  codesign --force --deep --options runtime --timestamp --sign "$MACOS_SIGN_IDENTITY" "$APP"
else
  codesign --force --deep --sign - "$APP"
fi
codesign --verify --deep --strict "$APP"

rm -rf dist && mkdir -p dist/dmg
ditto "$APP" "dist/dmg/FioTransfer.app"
ln -s /Applications dist/dmg/Applications
hdiutil create -quiet -volname "FioTransfer" -srcfolder dist/dmg -ov -format UDZO "dist/${NAME}.dmg"
rm -rf dist/dmg
ditto -c -k --keepParent "$APP" "dist/${NAME}-macOS.zip"

if [[ -n "${MACOS_SIGN_IDENTITY:-}" && -n "${APPLE_ID:-}" && -n "${APPLE_TEAM_ID:-}" && -n "${APPLE_APP_PASSWORD:-}" ]]; then
  codesign --force --sign "$MACOS_SIGN_IDENTITY" --timestamp "dist/${NAME}.dmg"
  xcrun notarytool submit "dist/${NAME}.dmg" --apple-id "$APPLE_ID" --team-id "$APPLE_TEAM_ID" \
    --password "$APPLE_APP_PASSWORD" --wait
  xcrun stapler staple "dist/${NAME}.dmg"
fi

(cd dist && shasum -a 256 * > SHA256SUMS.txt)
ls -lh dist
