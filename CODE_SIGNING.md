# Code signing policy

BTC09 Wallet is released under the [MIT license](LICENSE) from the public [BTC09 repository](https://github.com/krutftw/bitcoin09).

## Current platform trust

- Android releases are signed with a project release key kept outside the repository. The release publishes its certificate fingerprint, and updates must use the same key.
- Windows community builds are currently unsigned.
- macOS community builds are currently ad-hoc signed and not Apple-notarized.
- Linux AppImages are unsigned.

This means Windows and macOS can show an unknown-publisher or Gatekeeper warning. Users should download only from the official release, verify `SHA256SUMS`, and use the operating system's one-app override. BTC09 does not ask users to disable platform security.

Mining software and the full node are outside the wallet-only signing scope. Any future trusted signature for BTC09 Wallet must cover only the installer, wallet shell, and wallet-only core. The wallet-only core cannot start a miner or expose mining commands.

## Team roles

- Authors, committers, and reviewers: [krutftw](https://github.com/krutftw)
- Signing approver: [krutftw](https://github.com/krutftw)

Changes from other contributors require maintainer review before merge. Signing requests require a separate manual approval and must come from an immutable release tag. Multi-factor authentication is required for repository and signing access.

## Release checks

Every public wallet release must come from an immutable release tag. Automated builds verify the version, wallet-only boundary, package contents, embedded paths, and native interface. Release candidates are malware-scanned where supported, checksummed, uploaded, downloaded into a fresh folder, and compared with the published checksums before release.

If trusted Windows or Apple signing becomes available, the signing configuration must enforce the BTC09 Wallet product name and one consistent version across the package. The signing key must remain outside the repository and build runner. A request must be rejected if the source is not a release tag, required checks failed, or the artifact differs from the verified build output.

## Privacy and network use

Recovery words, private keys, wallet passwords, and unsigned transactions stay on the user's device. The wallet requests public blockchain data from btc09.org and sends a signed transaction only when the user chooses to broadcast it. The server and infrastructure boundaries are described in the [privacy policy](https://btc09.org/privacy.html).

The installer can ask Windows to obtain the Microsoft WebView2 runtime when the runtime is not already installed. Uninstallation is available through Windows Settings.

## Security reports

Report a suspected vulnerability through a private [GitHub Security Advisory](https://github.com/krutftw/bitcoin09/security/advisories/new).
