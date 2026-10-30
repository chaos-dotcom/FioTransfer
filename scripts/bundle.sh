#!/bin/bash
# Build FioTransfer.app into the project folder: the egui application, in a
# bundle with the Go app's Info.plist bytes, the icon and the notices, ad-hoc
# signed so macOS opens it after a download.
set -euo pipefail
cd "$(dirname "$0")/.."

PROFILE=${PROFILE:-release}
APP=FioTransfer.app

cargo build --profile "$PROFILE" -p fiotransfer-egui --bin fiotransfer-egui

BIN=target/$PROFILE/fiotransfer-egui
[ "$PROFILE" = dev ] && BIN=target/debug/fiotransfer-egui

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$BIN" "$APP/Contents/MacOS/FioTransfer"
# The Info.plist is the Go build's own file, byte for byte (minus the Finder
# services, which went with the webview shell).
cp build/darwin/Info.plist "$APP/Contents/Info.plist"
cp LICENSE "$APP/Contents/Resources/LICENSE.txt"
cp THIRD_PARTY_NOTICES.txt "$APP/Contents/Resources/"
cp build/icons/icon.icns "$APP/Contents/Resources/iconfile.icns"
cp build/appicon.png "$APP/Contents/Resources/appicon.png" 2>/dev/null || true
codesign --force --deep -s - "$APP" >/dev/null 2>&1
echo "Built ./$APP"
