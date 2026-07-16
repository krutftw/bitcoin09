# Direct wallet distribution design

Date: 16 July 2026
Status: approved for implementation

## Decision

BTC09 Wallet will be distributed from GitHub Releases and linked from btc09.org. The first native release will not depend on the Microsoft Store, Google Play, or Apple's stores.

The public files are:

- Windows x64 wallet-only installer, published only after trusted open-source code signing.
- Android ARM64 wallet-only APK, signed with the project's permanent release key.
- Linux x64 wallet-only AppImage.

macOS and iPhone are not part of this no-cost native release. A direct Mac build still produces Gatekeeper warnings without an Apple Developer ID, and free iPhone provisioning expires after seven days. Existing v0.1.32 command-line downloads remain available while native packaging is completed.

## Trust boundary

The direct packages contain the wallet shell and wallet-only core. They do not contain mining commands, demo wallets, recovery words, signing keys, debug endpoints, or analytics.

Windows signing will use SignPath Foundation if the project is accepted. The unsigned installer can be built and inspected, but it must not be promoted as the stable Windows download. Android's release key is generated once, kept outside the repository, backed up, and injected only into a protected release build. Linux packages are accompanied by SHA-256 checksums.

Every published artifact must be tied to an immutable Git tag. GitHub Releases is the source of record. btc09.org links to that release, and Discord receives one concise read-back-verified announcement.

## User experience

The website keeps v0.1.32 as the current release until a native file is actually available. A compact note in the download section reports the native wallet status without presenting unfinished files as downloads.

The public wording is direct:

- Windows: signing in progress.
- Android: signed APK in preparation.
- Linux: AppImage in preparation.
- Apple: later, because warning-free public distribution is not free.

Android users will still see the operating system's standard permission to install an APK from the browser or file manager. Project signing proves update continuity and file integrity; it does not remove Android's sideload permission prompt.

## Release gate

Before any v0.1.33 native file is announced:

1. Build the wallet-only edition from a clean tagged commit.
2. Verify that mining and demo content are absent.
3. Verify the platform signature where applicable.
4. Install and exercise create, restore, lock, receive, send, activity, backup, and cleanup on a clean device or VM.
5. Generate SHA256SUMS and compare the uploaded files with the local release set.
6. Read back the GitHub release, website links, and Discord post.

If a platform misses its gate, omit that platform. Do not delay or weaken the files that passed.
