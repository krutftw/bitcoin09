# BTC09 Wallet direct downloads

BTC09 Wallet is distributed directly through GitHub Releases. No app-store account is required.

## Available beta files

- **Android ARM64 signed APK:** [btc09-wallet-android-arm64.apk](https://github.com/krutftw/bitcoin09/releases/download/v0.1.34/btc09-wallet-android-arm64.apk)
- **Linux x64:** [btc09-wallet-linux-x64.AppImage](https://github.com/krutftw/bitcoin09/releases/download/v0.1.34/btc09-wallet-linux-x64.AppImage)
- **Checksums:** [SHA256SUMS](https://github.com/krutftw/bitcoin09/releases/download/v0.1.34/SHA256SUMS)

[Open the v0.1.34 release](https://github.com/krutftw/bitcoin09/releases/tag/v0.1.34). Android shows its standard sideload permission when an APK is installed from a browser or file manager. The APK is signed with the project release key.

## Windows and Mac preview

- **Windows 10/11 x64:** [btc09-wallet-windows-x64-setup.exe](https://github.com/krutftw/bitcoin09/releases/download/v0.1.34-wallet-preview.1/btc09-wallet-windows-x64-setup.exe)
- **macOS 13+, Apple silicon or Intel:** [btc09-wallet-macos-universal-preview.zip](https://github.com/krutftw/bitcoin09/releases/download/v0.1.34-wallet-preview.1/btc09-wallet-macos-universal-preview.zip)
- **Preview checksums:** [SHA256SUMS](https://github.com/krutftw/bitcoin09/releases/download/v0.1.34-wallet-preview.1/SHA256SUMS)

[Open the native preview release](https://github.com/krutftw/bitcoin09/releases/tag/v0.1.34-wallet-preview.1). These are real wallet-only apps, but Windows and macOS will warn that the publisher is not yet verified. The preview release gives the normal one-app override steps. Do not disable system security.

Windows trusted signing is still moving through the free SignPath Foundation route. The iPhone app passes simulator tests, but there is no normal free public installation path: free Apple-ID sideloads expire after seven days. Node and solo-miner operators should use the full v0.1.34 release.

## Code signing policy

Free code signing provided by SignPath.io, certificate by SignPath Foundation. Read about [SignPath](https://signpath.io/) or open the full [Code signing policy](../CODE_SIGNING.md) for the signed scope, team roles, approval rules, privacy behavior, and security-reporting path.

## What wallet-only means

The native package can create or restore a wallet, lock it, receive, review and send payments, show activity, back up the wallet, and combine small wallet outputs. It does not contain the node or mining commands. Mining remains available in the full BTC09 desktop release.

## Verify a download

Official files are attached to [GitHub Releases](https://github.com/krutftw/bitcoin09/releases). Each release includes `SHA256SUMS`.

On Android, the project publishes the release certificate fingerprint with the APK and uses the same key for future updates. On Linux, verify the AppImage checksum before making it executable. Windows and Mac preview users should verify the separate preview checksum file before opening the app.

Do not use a wallet file sent through a direct message, a mirror, or a file-sharing link. BTC09 maintainers will never ask for recovery words, private keys, or a wallet password.

## Release gate

Stable downloads require trusted platform signing. A clearly labeled preview can be published after public-CI build verification, artifact inspection, malware scanning where available, checksum generation, upload, and download read-back. Preview files never replace a trusted build when one becomes available.

See the public [Code signing policy](../CODE_SIGNING.md) for the Windows trust process.
