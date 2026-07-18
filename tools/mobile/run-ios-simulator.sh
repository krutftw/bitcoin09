#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
APPLE_DIR="$ROOT/walletapp/src-tauri/gen/apple"
BUILD_LOG="$APPLE_DIR/build/ci-simulator-build.log"

case "$(uname -m)" in
  arm64) expected_arch="arm64" ;;
  x86_64) expected_arch="x86_64" ;;
  *) echo "Unsupported macOS CI architecture: $(uname -m)" >&2; exit 1 ;;
esac

mkdir -p "$(dirname "$BUILD_LOG")"

# Tauri owns the short-lived settings server used by its Xcode build phase.
# It currently continues from a successful simulator build into an archive,
# which needs a paid Apple development team. Accept only that exact boundary.
set +e
(
  cd "$ROOT/walletapp"
  npm run tauri -- ios build --debug --target aarch64-sim --ci
) > "$BUILD_LOG" 2>&1
tauri_status=$?
set -e

if ! grep -q '\*\* BUILD SUCCEEDED \*\*' "$BUILD_LOG"; then
  cat "$BUILD_LOG" >&2
  if (( tauri_status == 0 )); then exit 1; fi
  exit "$tauri_status"
fi
if (( tauri_status != 0 )); then
  if ! grep -q 'Signing for "btc09-wallet_iOS" requires a development team' "$BUILD_LOG" \
    || ! grep -q '\*\* ARCHIVE FAILED \*\*' "$BUILD_LOG"; then
    cat "$BUILD_LOG" >&2
    exit "$tauri_status"
  fi
  echo "Simulator build passed; Apple distribution archive is unavailable in the free CI gate."
fi

APP="$(find "$HOME/Library/Developer/Xcode/DerivedData" "$APPLE_DIR/build" \
  -type d -path '*/debug-iphonesimulator/BTC09 Wallet.app' -print -quit 2>/dev/null || true)"
test -n "$APP"
APP_BINARY="$APP/BTC09 Wallet"

test -x "$APP_BINARY"
actual_archs="$(lipo -archs "$APP_BINARY")"
case " $actual_archs " in
  *" $expected_arch "*) ;;
  *) echo "Simulator app is missing $expected_arch: $actual_archs" >&2; exit 1 ;;
esac

test "$(plutil -extract CFBundleShortVersionString raw "$APP/Info.plist")" = "0.1.34"
symbols="$APPLE_DIR/build/app-symbols.txt"
nm -gU "$APP_BINARY" > "$symbols"
grep -q ' _MobilewalletNewEngine$' "$symbols"
echo "Verified native BTC09 iPhone simulator app ($expected_arch, wallet core linked)."
