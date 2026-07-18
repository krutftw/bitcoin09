# BTC09 Wallet v0.1.34 Linux maintenance build 1

This replaces the Linux AppImage attached to the main v0.1.34 release.

## What changed

The wallet now includes the gtk-rs maintainers' fix for
[GHSA-wrw7-89jp-8q8g](https://github.com/advisories/GHSA-wrw7-89jp-8q8g).
BTC09's Tauri/GTK stack still requires the glib 0.18 crate line, so the exact
upstream fix is backported onto the official glib 0.18.5 source and pinned in
the build.

Wallet files, addresses, balances, transactions, recovery words, chain rules,
and the wallet interface are unchanged.

## Who should update

Anyone using `btc09-wallet-linux-x64.AppImage` should download this maintenance
build and replace the previous AppImage. The BTC09 node and miner binaries,
Android wallet, Windows wallet, and Mac wallet do not use this Linux GTK
dependency and do not need an update for this issue.

## Verify the download

Download `btc09-wallet-linux-x64.AppImage` and `SHA256SUMS` from this release,
then run:

```text
sha256sum -c SHA256SUMS
```

Only use files published by the
[official BTC09 repository](https://github.com/krutftw/bitcoin09/releases).
