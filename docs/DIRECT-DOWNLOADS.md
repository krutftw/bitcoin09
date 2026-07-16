# BTC09 Wallet direct downloads

BTC09 Wallet is distributed directly through GitHub Releases. No app-store account is required.

## Available beta files

- **Android ARM64 signed APK:** [btc09-wallet-android-arm64.apk](https://github.com/krutftw/bitcoin09/releases/download/v0.1.33-beta.1/btc09-wallet-android-arm64.apk)
- **Linux x64:** [btc09-wallet-linux-x64.AppImage](https://github.com/krutftw/bitcoin09/releases/download/v0.1.33-beta.1/btc09-wallet-linux-x64.AppImage)
- **Checksums:** [SHA256SUMS](https://github.com/krutftw/bitcoin09/releases/download/v0.1.33-beta.1/SHA256SUMS)

[Open the v0.1.33 beta release](https://github.com/krutftw/bitcoin09/releases/tag/v0.1.33-beta.1). Android shows its standard sideload permission when an APK is installed from a browser or file manager. The APK is signed with the project release key.

Windows is waiting for free signing through SignPath Foundation; an unsigned installer is not published. macOS and iPhone are not included in this beta. v0.1.32 remains the stable full app for Windows, macOS, mining, and full-node use.

## What wallet-only means

The native package can create or restore a wallet, lock it, receive, review and send payments, show activity, back up the wallet, and combine small wallet outputs. It does not contain the node or mining commands. Mining remains available in the full BTC09 desktop release.

## Verify a download

Official files are attached to [GitHub Releases](https://github.com/krutftw/bitcoin09/releases). Each release includes `SHA256SUMS`.

On Android, the project publishes the release certificate fingerprint with the APK and uses the same key for future updates. On Linux, verify the AppImage checksum before making it executable.

Do not use a wallet file sent through a direct message, a mirror, or a file-sharing link. BTC09 maintainers will never ask for recovery words, private keys, or a wallet password.

## Release gate

The project does not announce a native platform until its final file has passed artifact inspection, signature verification, a clean install, the wallet flow, checksum generation, upload, and download read-back. A platform that has not passed remains listed as in progress rather than downloadable.

See the public [code signing policy](../CODE_SIGNING.md) for the Windows trust process.
