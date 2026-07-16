# Code signing policy

Free code signing provided by [SignPath.io](https://signpath.io/), certificate by [SignPath Foundation](https://signpath.org/).

## Project

This policy covers the Windows wallet-only packages built from the public [BTC09 repository](https://github.com/krutftw/bitcoin09). BTC09 Wallet is released under the [MIT license](LICENSE).

Mining software is outside this signing policy. Signing requests may cover the BTC09 Wallet installer, its wallet shell, and its wallet-only core. The signed edition cannot start a miner or expose mining commands.

## Team roles

- Authors, committers, and reviewers: [krutftw](https://github.com/krutftw)
- Signing approver: [krutftw](https://github.com/krutftw)

Changes from other contributors require maintainer review before merge. Signing requests require a separate manual approval and must come from an immutable release tag. Multi-factor authentication is required for repository and signing access.

## Signing requests

The release workflow builds from the public repository, checks the wallet-only boundary, and submits the resulting artifacts to SignPath. Signing keys are generated and held by the signing provider and are never available to the repository or build runner.

The signing configuration must enforce the BTC09 Wallet product name and one consistent product version across the installer and signed executable files. A signing request is rejected if the source revision is not a release tag, required checks failed, or the artifact differs from the automated build output.

## Privacy and network use

Recovery words, private keys, wallet passwords, and unsigned transactions stay on the user's device. The wallet requests public blockchain data from btc09.org and sends a signed transaction only when the user chooses to broadcast it. The server and infrastructure boundaries are described in the [privacy policy](https://btc09.org/privacy.html).

The installer can ask Windows to obtain the Microsoft WebView2 runtime when the runtime is not already installed. Uninstallation is available through Windows Settings.

## Security reports

Report a suspected vulnerability through a private [GitHub Security Advisory](https://github.com/krutftw/bitcoin09/security/advisories/new). Report a suspected misuse of a SignPath Foundation certificate to support@signpath.io with the affected file and supporting evidence.
