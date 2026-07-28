#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

export CI=true

sudo apt-get update
sudo apt-get install -y --no-install-recommends \
  build-essential \
  ca-certificates \
  curl \
  dbus-x11 \
  file \
  libasound2t64 \
  libayatana-appindicator3-dev \
  libgtk-3-0 \
  librsvg2-dev \
  libssl-dev \
  libwebkit2gtk-4.1-dev \
  libxdo-dev \
  patchelf \
  x11-utils \
  xvfb \
  xz-utils
sudo rm -rf /var/lib/apt/lists/*

curl --fail --location --silent --show-error \
  https://go.dev/dl/go1.25.12.linux-amd64.tar.gz \
  --output /tmp/go.tgz
echo "234828b7a89e0e303d2556310ee549fbcf253d28de937bac3da13d6294262ac1  /tmp/go.tgz" |
  sha256sum --check
sudo mkdir -p /opt/go1.25.12
sudo tar -C /opt/go1.25.12 --strip-components=1 -xzf /tmp/go.tgz

curl --fail --location --silent --show-error \
  https://nodejs.org/dist/v24.11.1/node-v24.11.1-linux-x64.tar.xz \
  --output /tmp/node.tar.xz
echo "60e3b0a8500819514aca603487c254298cd776de0698d3cd08f11dba5b8289a8  /tmp/node.tar.xz" |
  sha256sum --check
sudo mkdir -p /opt/node24
sudo tar -C /opt/node24 --strip-components=1 -xf /tmp/node.tar.xz

export GOROOT="/opt/go1.25.12"
export GOTOOLCHAIN=local
export PATH="$GOROOT/bin:/opt/node24/bin:$HOME/.cargo/bin:$PATH"
if ! command -v rustup >/dev/null 2>&1; then
  curl --proto '=https' --tlsv1.2 --silent --show-error --fail \
    https://sh.rustup.rs \
    --output /tmp/rustup-init.sh
  sh /tmp/rustup-init.sh \
    -y \
    --profile minimal \
    --default-toolchain none
fi
rustup toolchain install 1.95.0 --profile minimal --no-self-update
rustup default 1.95.0
rustup component add rustfmt

go version
test "$(go env GOROOT)" = "$GOROOT"
node --version
rustc --version

npm ci --prefix walletapp
go vet ./...
go test ./...
cargo fmt --manifest-path walletapp/src-tauri/Cargo.toml -- --check
node tools/desktop/prepare-sidecar.mjs wallet
cargo test --manifest-path walletapp/src-tauri/Cargo.toml

npm --prefix walletapp run store:build -- --bundles appimage
node tools/desktop/verify-wallet-edition.mjs \
  walletapp/src-tauri/target/release/btc09-core

bundle_dir="walletapp/src-tauri/target/release/bundle/appimage"
source_appimage="$(find "$bundle_dir" -maxdepth 1 -type f -name '*.AppImage' -print -quit)"
test -n "$source_appimage"
test -s "$source_appimage"

direct_dir="walletapp/src-tauri/target/direct"
release_appimage="$direct_dir/btc09-wallet-linux-x64.AppImage"
mkdir -p "$direct_dir"
cp "$source_appimage" "$release_appimage"
chmod +x "$release_appimage"
sha256sum "$release_appimage"

runtime_dir="$(mktemp -d)"
chmod 700 "$runtime_dir"
xvfb_pid=""
wallet_pid=""
cleanup() {
  if [[ -n "$wallet_pid" ]]; then
    kill "$wallet_pid" 2>/dev/null || true
  fi
  if [[ -n "$xvfb_pid" ]]; then
    kill "$xvfb_pid" 2>/dev/null || true
  fi
  rm -rf "$runtime_dir"
}
trap cleanup EXIT

Xvfb :99 -screen 0 1280x800x24 >/tmp/btc09-xvfb.log 2>&1 &
xvfb_pid=$!
display_ready=0
for _ in $(seq 1 30); do
  if DISPLAY=:99 xdpyinfo >/dev/null 2>&1; then
    display_ready=1
    break
  fi
  sleep 0.5
done
test "$display_ready" = 1

DISPLAY=:99 XDG_RUNTIME_DIR="$runtime_dir" \
  dbus-run-session -- env APPIMAGE_EXTRACT_AND_RUN=1 "$release_appimage" \
  >/tmp/btc09-wallet.log 2>&1 &
wallet_pid=$!

window_found=0
for _ in $(seq 1 40); do
  if ! kill -0 "$wallet_pid" 2>/dev/null; then
    cat /tmp/btc09-wallet.log >&2
    exit 1
  fi
  if DISPLAY=:99 xwininfo -root -tree >/tmp/btc09-windows.txt 2>&1 &&
    grep -F "BTC09 Wallet" /tmp/btc09-windows.txt; then
    window_found=1
    break
  fi
  sleep 0.5
done
test "$window_found" = 1
echo "Verified clean Linux launch: BTC09 Wallet"
