# Bitcoin 09 v0.1.32

This release makes the desktop wallet easier to understand and use without a
terminal.

## Wallet activity

- Activity shows recent sends, received payments, mining rewards, pending
  transactions, and confirmation or maturity status.
- Every activity entry can copy its TXID or open the transaction in the public
  explorer.
- The main balance is now the amount ready to send. Mining rewards that still
  need confirmations are shown separately.

## Sending and small payments

- Max asks the wallet to calculate the largest safe send after the chosen fee.
  The review still shows the exact destination, amount, fee, total, selected
  payment count, chain height, and check code before broadcast.
- Combine small payments is an optional same-wallet transaction for wallets
  that have collected many small mining or receive outputs. It selects a
  bounded batch, shows the count and fee, and requires a separate confirmation.
- Going back from a payment or cleanup review now releases its selected inputs
  immediately.

Cleanup does not create coins, increase the wallet balance, or increase mining
rewards. It pays the network fee shown in the review so later sends can use
fewer inputs.

## Compatibility and upgrade

There are no consensus, proof-of-work, P2P, supply, mining-rule, or wallet-file
format changes. The existing light-wallet snapshot API is unchanged for older
strict clients; the new wallet view endpoint adds spendable, immature, and
activity data separately.

1. Close any running BTC09 process.
2. Keep your recovery words offline and make a current wallet backup before
   replacing an older executable. Never send either backup to support.
3. Download the correct v0.1.32 file and `SHA256SUMS` from the official GitHub
   release.
4. Verify the checksum, replace the old executable, and open the wallet.

Existing wallets remain readable. The upgrade does not rewrite them.

## Release files

| Platform | File |
| --- | --- |
| Windows x64 | `btc09-windows-amd64.exe` |
| Linux x64 | `btc09-linux-amd64` |
| Linux arm64 | `btc09-linux-arm64` |
| macOS Apple silicon | `btc09-macos-apple.zip` |
| macOS Intel | `btc09-macos-intel.zip` |
| Checksums | `SHA256SUMS` |

Windows PowerShell:

```powershell
Get-FileHash .\btc09-windows-amd64.exe -Algorithm SHA256
```

Linux or macOS:

```text
sha256sum -c SHA256SUMS
```

Compare only files you downloaded. Checksums are generated from the final
published artifact bytes.
