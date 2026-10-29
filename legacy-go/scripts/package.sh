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

# The third-party licenses must ship with the app; they're generated from what
# this build compiled in, so a dependency change can't leave them stale.
cp LICENSE "$APP/Contents/Resources/LICENSE.txt"
scripts/notices.sh > "$APP/Contents/Resources/THIRD_PARTY_NOTICES.txt"
if ! cmp -s "$APP/Contents/Resources/THIRD_PARTY_NOTICES.txt" THIRD_PARTY_NOTICES.txt; then
  echo "::warning::THIRD_PARTY_NOTICES.txt is out of date; run scripts/notices.sh > THIRD_PARTY_NOTICES.txt"
fi

NOTARIZE=false
if [[ -n "${MACOS_SIGN_IDENTITY:-}" && -n "${APPLE_ID:-}" && -n "${APPLE_TEAM_ID:-}" && -n "${APPLE_APP_PASSWORD:-}" ]]; then
  NOTARIZE=true
fi

# Submit to Apple's notary service; print the log and fail unless Accepted.
notarize() {
  local out id status
  out="$(xcrun notarytool submit "$1" --apple-id "$APPLE_ID" --team-id "$APPLE_TEAM_ID" \
    --password "$APPLE_APP_PASSWORD" --wait --output-format json)"
  id="$(plutil -extract id raw -o - - <<<"$out")"
  status="$(plutil -extract status raw -o - - <<<"$out")"
  echo "notarization $id: $status"
  if [[ "$status" != "Accepted" ]]; then
    xcrun notarytool log "$id" --apple-id "$APPLE_ID" --team-id "$APPLE_TEAM_ID" --password "$APPLE_APP_PASSWORD"
    exit 1
  fi
}

if [[ -n "${MACOS_SIGN_IDENTITY:-}" ]]; then
  codesign --force --deep --options runtime --timestamp --sign "$MACOS_SIGN_IDENTITY" "$APP"
else
  codesign --force --deep --sign - "$APP"
fi
codesign --verify --deep --strict "$APP"

# Notarize and staple the app itself first, so the copy in the .dmg and the
# .zip both carry the ticket and open without an online check.
if $NOTARIZE; then
  rm -rf build/notarize && mkdir -p build/notarize
  ditto -c -k --keepParent "$APP" build/notarize/FioTransfer.zip
  notarize build/notarize/FioTransfer.zip
  xcrun stapler staple "$APP"
  rm -rf build/notarize
fi

rm -rf dist && mkdir -p dist/dmg
ditto "$APP" "dist/dmg/FioTransfer.app"
ln -s /Applications dist/dmg/Applications
hdiutil create -quiet -volname "FioTransfer" -srcfolder dist/dmg -ov -format UDZO "dist/${NAME}.dmg"
rm -rf dist/dmg
ditto -c -k --keepParent "$APP" "dist/${NAME}-macOS.zip"

if $NOTARIZE; then
  codesign --force --sign "$MACOS_SIGN_IDENTITY" --timestamp "dist/${NAME}.dmg"
  notarize "dist/${NAME}.dmg"
  xcrun stapler staple "dist/${NAME}.dmg"
  spctl -a -vv -t exec "$APP"
  spctl -a -vv -t open --context context:primary-signature "dist/${NAME}.dmg"
fi

(cd dist && shasum -a 256 * > SHA256SUMS.txt)
ls -lh dist
