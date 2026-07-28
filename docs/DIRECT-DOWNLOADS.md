# BTC09 Wallet direct downloads

BTC09 Wallet is distributed through the official [v0.1.35 GitHub release](https://github.com/krutftw/bitcoin09/releases/tag/v0.1.35). No app-store account is required.

## Wallet apps

- **Windows 10/11 x64:** [btc09-wallet-windows-x64-setup.exe](https://github.com/krutftw/bitcoin09/releases/download/v0.1.35/btc09-wallet-windows-x64-setup.exe)
- **macOS 13+, Apple silicon or Intel:** [btc09-wallet-macos-universal-preview.zip](https://github.com/krutftw/bitcoin09/releases/download/v0.1.35/btc09-wallet-macos-universal-preview.zip)
- **Android ARM64 signed APK:** [btc09-wallet-android-arm64.apk](https://github.com/krutftw/bitcoin09/releases/download/v0.1.35/btc09-wallet-android-arm64.apk)
- **Linux x64 AppImage:** [btc09-wallet-linux-x64.AppImage](https://github.com/krutftw/bitcoin09/releases/download/v0.1.35/btc09-wallet-linux-x64.AppImage)
- **Checksums:** [SHA256SUMS](https://github.com/krutftw/bitcoin09/releases/download/v0.1.35/SHA256SUMS)

Android shows its standard sideload permission when an APK is installed from a browser or file manager. It is signed with the same project release key as v0.1.34, so normal upgrades keep working.

Windows and macOS are verified community builds, but they are not publisher-signed or Apple-notarized. Windows SmartScreen or macOS Gatekeeper may warn on first launch. Verify `SHA256SUMS`, then use the operating system's one-app override. Do not disable system security.

The iPhone build passes simulator tests, but there is no durable free public install. Free Apple-ID sideloads expire after seven days.

## Wallet app or full client?

The wallet app can create or restore a wallet, lock it, receive, review and send payments, show activity, back up the wallet, and combine small wallet outputs. It cannot run a node or miner.

Use the full BTC09 client from the same release if you want to mine, relay the chain, run an explorer, or use command-line tools. v0.1.35 does not change consensus; v0.1.34 nodes remain compatible.

## Verify a download

Official files are attached to [GitHub Releases](https://github.com/krutftw/bitcoin09/releases). Compare the file's SHA-256 hash with `SHA256SUMS` before opening it. The release also includes build provenance and the Android signing-certificate fingerprint.

Do not use a wallet file sent through a direct message, a mirror, or a file-sharing link. BTC09 maintainers will never ask for recovery words, private keys, or a wallet password.

See the public [code signing policy](../CODE_SIGNING.md) for current platform trust and release checks.
