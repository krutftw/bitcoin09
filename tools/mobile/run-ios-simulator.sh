#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
APPLE_DIR="$ROOT/walletapp/src-tauri/gen/apple"
PROJECT="$APPLE_DIR/btc09-wallet.xcodeproj"
DERIVED_DATA="$APPLE_DIR/build/ci-simulator-derived-data"
APP="$DERIVED_DATA/Build/Products/debug-iphonesimulator/BTC09 Wallet.app"
APP_BINARY="$APP/BTC09 Wallet"

case "$(uname -m)" in
  arm64) expected_arch="arm64" ;;
  x86_64) expected_arch="x86_64" ;;
  *) echo "Unsupported macOS CI architecture: $(uname -m)" >&2; exit 1 ;;
esac

test -d "$PROJECT"
rm -rf "$DERIVED_DATA"

# `tauri ios build` always continues into an archive, which requires an Apple
# development team. The free gate needs the real simulator app and native link,
# not a distributable archive.
xcodebuild \
  -project "$PROJECT" \
  -scheme btc09-wallet_iOS \
  -configuration debug \
  -sdk iphonesimulator \
  -destination "generic/platform=iOS Simulator" \
  -derivedDataPath "$DERIVED_DATA" \
  ARCHS="$expected_arch" \
  CODE_SIGNING_ALLOWED=NO \
  build

test -x "$APP_BINARY"
actual_archs="$(lipo -archs "$APP_BINARY")"
case " $actual_archs " in
  *" $expected_arch "*) ;;
  *) echo "Simulator app is missing $expected_arch: $actual_archs" >&2; exit 1 ;;
esac

test "$(plutil -extract CFBundleShortVersionString raw "$APP/Info.plist")" = "0.1.34"
nm -gU "$APP_BINARY" | grep -q ' _MobilewalletNewEngine$'
echo "Verified native BTC09 iPhone simulator app ($expected_arch, wallet core linked)."
