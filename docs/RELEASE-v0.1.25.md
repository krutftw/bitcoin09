# Bitcoin 09 v0.1.25

This release puts an official open-source CPU miner inside the BTC09 desktop wallet.

## Wallet miner

- Open the wallet, choose **Mine**, set the CPU thread count, and start or stop mining without a terminal.
- Fast mode mines against the official HTTPS coordinator without downloading the chain.
- The wallet fills its own payout address. Private keys and the wallet file never leave the computer.
- The panel shows current and session-average hashrate, hashes, jobs, reconnects, accepted blocks, and plain connection errors.
- Temporary transport and server faults reconnect with bounded backoff.

## Honest payout model

The official service is remote solo, not a pooled-share service. It has no pool balance or partial-share payout. A network-winning block pays the miner's wallet directly. Solo results have high variance.

The existing community pool and NTMminer remain third-party services. The wallet does not download, redistribute, or depend on their closed-source miner.

## Open protocol and deployment

- `docs/OPEN-MINING-PROTOCOL.md` documents the two-route JSON protocol for independent miners and coordinators.
- The official coordinator is available at `https://btc09.org`.
- The origin listens only on `127.0.0.1:9010`. Cloudflare and nginx expose the two exact POST routes with body, connection, request-rate, and timeout limits.

This release does not change consensus, block validation, the P2P protocol, or the emission schedule.

## Verification

Release artifacts are accompanied by `SHA256SUMS`. Verify the checksum before running a downloaded binary.
