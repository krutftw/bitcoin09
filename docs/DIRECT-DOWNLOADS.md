# BTC09 Wallet direct downloads

BTC09 Wallet will use GitHub Releases instead of the Microsoft Store, Google Play, or Apple's stores. v0.1.32 remains the current release until each native package has passed its release gate.

## Planned native files

- **Windows x64:** a wallet-only installer signed through SignPath Foundation. An unsigned preflight file is not a public release.
- **Android ARM64:** a wallet-only signed APK. Android shows its standard sideload permission when an APK is installed from a browser or file manager. This is expected even when the APK has a valid project signature.
- **Linux x64:** a wallet-only AppImage that can run without a system-wide installation.

The native release does not include macOS or iPhone. Warning-free public Mac distribution needs a paid Apple Developer ID. Free iPhone provisioning expires after seven days and is not a practical public download.

## What wallet-only means

The native package can create or restore a wallet, lock it, receive, review and send payments, show activity, back up the wallet, and combine small wallet outputs. It does not contain the node or mining commands. Mining remains available in the full BTC09 desktop release.

## Verify a download

Official files are attached to an immutable release at [GitHub Releases](https://github.com/krutftw/bitcoin09/releases). Each release includes `SHA256SUMS`.

On Windows, open the installer's Properties and check Digital Signatures before running it. The expected publisher for the free signing route is SignPath Foundation. On Android, the project publishes the release certificate fingerprint with the APK and uses the same key for future updates.

Do not use a wallet file sent through a direct message, a mirror, or a file-sharing link. BTC09 maintainers will never ask for recovery words, private keys, or a wallet password.

## Release gate

The project does not announce a native platform until its final file has passed artifact inspection, signature verification, a clean install, the wallet flow, checksum generation, upload, and download read-back. A platform that has not passed remains listed as in progress rather than downloadable.

See the public [code signing policy](../CODE_SIGNING.md) for the Windows trust process.
