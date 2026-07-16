# Bitcoin 09 v0.1.34

## Mandatory network update

Node and solo-miner operators must update before height 12,096. At that block,
BTC09 switches from the legacy 2,016-block difficulty window to per-block ASERT
with a two-hour half-life.

This removes the large adjustment cliff that appears when network hashrate
changes quickly. Difficulty starts moving every block instead of waiting for
the end of a full window.

Pool miners do not need to change their address, worker name, or mining
settings. The upgraded pool sends current work automatically. Pool operators
must upgrade their node before activation.

## What stays the same

Wallet files and addresses do not change. Existing balances, transactions,
recovery words, supply, block rewards, Argon2id proof of work, and the 10-minute
target remain the same.

Blocks below height 12,096 continue to use the original rules. From activation,
block timestamps must be later than the median of the prior 11 blocks and no
more than 30 minutes ahead of network time.

## Upgrade

1. Close BTC09 and keep a current wallet backup. Never send a wallet file,
   password, private key, or recovery words to support.
2. Download the correct v0.1.34 file and `SHA256SUMS` from the official GitHub
   release.
3. Verify the checksum and replace the previous application or executable.
4. Start BTC09 normally. Wallet users do not need to restore or create a new
   wallet.

## Release files

| Platform | File |
| --- | --- |
| Windows x64 full app, node, and miner | `btc09-windows-amd64.exe` |
| Linux x64 full app, node, and miner | `btc09-linux-amd64` |
| Linux arm64 full app, node, and miner | `btc09-linux-arm64` |
| macOS Apple silicon full app | `btc09-macos-apple.zip` |
| macOS Intel full app | `btc09-macos-intel.zip` |
| Android arm64 wallet | `btc09-wallet-android-arm64.apk` |
| Linux x64 native wallet | `btc09-wallet-linux-x64.AppImage` |
| Checksums | `SHA256SUMS` |

The wallet-only Windows installer will be added after its free SignPath signing
request completes. It will not contain node or mining features.

Windows PowerShell:

```powershell
Get-FileHash .\btc09-windows-amd64.exe -Algorithm SHA256
```

Linux or macOS:

```text
sha256sum -c SHA256SUMS
```

Compare only the files you downloaded. Checksums are generated from the final
published bytes.
