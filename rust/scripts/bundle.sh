#!/bin/bash
# Build FioTransfer.app into the project folder, the same bundle the Go
# Makefile produces: same Info.plist bytes, same resources, ad-hoc signed.
set -euo pipefail
cd "$(dirname "$0")/.."

PROFILE=${PROFILE:-release}
APP=FioTransfer.app

cargo build --profile "$PROFILE" --bin FioTransfer

BIN=target/$PROFILE/FioTransfer
[ "$PROFILE" = dev ] && BIN=target/debug/FioTransfer

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$BIN" "$APP/Contents/MacOS/FioTransfer"
# The Info.plist is the Go build's own file, byte for byte.
cp build/darwin/Info.plist "$APP/Contents/Info.plist"
cp LICENSE "$APP/Contents/Resources/LICENSE.txt"
cp THIRD_PARTY_NOTICES.txt "$APP/Contents/Resources/"
cp build/appicon.png "$APP/Contents/Resources/appicon.png" 2>/dev/null || true
codesign --force --deep -s - "$APP" >/dev/null 2>&1
echo "Built ./$APP"
