# Bitcoin 09 v0.1.35

v0.1.35 is an application and usability release. It does not change consensus,
proof of work, supply, rewards, addresses, wallet files, or the ASERT rules
activated at height 12,096.

## Wallet

- Rebuilt the Windows, macOS, Linux, Android, and iPhone wallet interface around
  the same clear receive, send, activity, backup, and cleanup flow.
- Uses the full desktop window on computers instead of presenting a phone-sized
  page inside it.
- Keeps the balance, network state, and next safe action visible.
- Shortens setup and payment copy while preserving recovery-word confirmation
  and transaction review.
- Keeps private keys, recovery words, passwords, and signing on the device.

## Desktop client

- The full desktop client keeps its CPU miner and node tools.
- The wallet-only edition cannot start a miner and does not contain mining
  commands.
- Native package checks now exercise the actual embedded desktop interface,
  including onboarding, receive, send, activity, and the optional full-client
  miner.

## Upgrade notes

- Existing wallet files and addresses continue to work.
- Back up the wallet file or recovery words before upgrading.
- This is not a mandatory network fork. Nodes running v0.1.34 remain
  consensus-compatible, although v0.1.35 is recommended for the app fixes.
- Install only files attached to the official GitHub release and verify
  `SHA256SUMS`.

## Release files

- Wallet apps: Windows installer, universal macOS ZIP, Linux AppImage, and
  signed Android ARM64 APK.
- Full clients: Windows x64, Linux x64 and ARM64, and macOS Apple silicon and
  Intel.
- `SHA256SUMS`, build provenance, and the Android signing-certificate
  fingerprint are attached to the release.
