# Bitcoin 09 v0.1.26

This release packages Nine Inbox into the downloadable desktop wallet and
makes the official miner easier to troubleshoot.

## Desktop wallet

- The wallet links directly to Nine Inbox for moving notes, links, photos, and
  files between a phone and computer.
- Nine Inbox remains optional. It does not need an account or 09C, and item
  contents are encrypted in the browser before upload.
- Windows, Linux, and macOS binaries are built with Go 1.25.12.

## Miner help

- The Mine tab shows wallet, endpoint, and CPU readiness before and during a
  session.
- Thread guidance makes the leave-one-thread-free default clear.
- **Copy help report** collects the version, wallet mode, miner state, thread
  count, hashrate, jobs, reconnects, runtime, job height, and last public error.
- The help report deliberately excludes the payout address, worker name,
  wallet path, keys, and wallet contents.
- Fatal coordinator responses now tell the user to update and share the safe
  report instead of showing a generic retry message.

Open solo remains remote solo mining. There are no partial-share payouts, and
this release does not change consensus, block validation, the P2P protocol,
emission, or wallet ownership.

## Verification

Release artifacts are accompanied by `SHA256SUMS`. Verify the checksum before
running a downloaded binary.
