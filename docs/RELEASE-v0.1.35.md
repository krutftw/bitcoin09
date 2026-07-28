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

## Planned release files

- `btc09-wallet-windows-x64-setup.exe`
- `btc09-wallet-macos-universal-preview.zip`
- `btc09-wallet-linux-x64.AppImage`
- `btc09-wallet-android-arm64.apk`
- `btc09-windows-amd64.exe`
- `btc09-linux-amd64`
- `btc09-linux-arm64`
- `btc09-macos-apple.zip`
- `btc09-macos-intel.zip`
- `SHA256SUMS`
