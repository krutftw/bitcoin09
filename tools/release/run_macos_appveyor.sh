#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

export CI=true
export GOTOOLCHAIN=auto

brew install go node@24 cocoapods
export PATH="$(brew --prefix node@24)/bin:$HOME/.cargo/bin:$PATH"

if ! command -v rustup >/dev/null 2>&1; then
  curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs |
    sh -s -- -y --profile minimal --default-toolchain none
fi

rustup toolchain install 1.95.0 --profile minimal --no-self-update
rustup default 1.95.0
rustup component add rustfmt
rustup target add \
  x86_64-apple-darwin \
  aarch64-apple-darwin \
  x86_64-apple-ios \
  aarch64-apple-ios-sim

cargo_home="${CARGO_HOME:-$HOME/.cargo}"
rustup_home="${RUSTUP_HOME:-$HOME/.rustup}"
separator=$'\x1f'
export CARGO_ENCODED_RUSTFLAGS="--remap-path-prefix=$repo_root=/src/bitcoin09${separator}--remap-path-prefix=$cargo_home=/cargo${separator}--remap-path-prefix=$rustup_home=/rustup"

go version
node --version
rustc --version
xcodebuild -version

npm ci --prefix walletapp
go vet ./...
go test ./...

cargo fmt --manifest-path walletapp/src-tauri/Cargo.toml -- --check
npm --prefix walletapp run mobile:core:ios
(
  cd walletapp
  npm run tauri -- ios init --ci --skip-targets-install
  npm run mobile:ios:simulator
)

node tools/desktop/prepare-sidecar.mjs wallet
cargo test --manifest-path walletapp/src-tauri/Cargo.toml

APPLE_SIGNING_IDENTITY=- npm --prefix walletapp run macos:universal:build
node tools/desktop/verify-macos-bundle.mjs \
  "walletapp/src-tauri/target/universal-apple-darwin/release/bundle/macos/BTC09 Wallet.app"
