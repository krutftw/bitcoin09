# Bitcoin 09 Quickstart

Bitcoin 09 is a fair-launch CPU proof-of-work coin. There is no premine, no
ICO, no allocation, and the genesis reward is burned.

## Download

Use the latest release:

https://github.com/krutftw/bitcoin09/releases/latest

Official site:

https://btc09.org

Windows PowerShell note: if you downloaded `btc09-windows-amd64.exe` and did
not rename it, run it with `.\btc09-windows-amd64.exe`. `btc09` by itself only
works after renaming the file to `btc09.exe` and using `.\btc09.exe`, or after
adding it to your PATH.

Check your binary:

```powershell
.\btc09-windows-amd64.exe version
```

The examples below use `btc09` for readability. On Windows, replace `btc09`
with `.\btc09-windows-amd64.exe` unless you renamed or installed the binary.

Current release:

```text
Bitcoin 09 (09C) reference node v0.1.30
```

## Open the Desktop Wallet

Double-click the downloaded BTC09 program, or run it without a command:

```powershell
.\btc09-windows-amd64.exe
```

On macOS, download the ZIP for Apple silicon or Intel, unzip it, and open
`Bitcoin 09.app`. If macOS blocks the first launch, verify the ZIP against
`SHA256SUMS`, then right-click the app and choose **Open**. The current
community build is not Apple-notarized.

BTC09 opens a local wallet interface in your normal browser. Fast mode shows
your balance without downloading the full chain and still signs payments only
on this computer. A new wallet gives you 24 recovery words and encrypts its
local file with the password you choose. It can restore the same receive
address from those words, show a receive QR code, and review and send payments.
It binds only to `127.0.0.1` on a random port. Passwords, recovery words, and
private keys stay on this computer.

Write the 24 words down in order and keep them offline. Anyone with the words
can spend the wallet, and the project cannot recover them. The app checks three
words before setup finishes. It also supports an encrypted file backup and
never overwrites an existing backup file. Existing V1 wallets still open with
their original addresses and must keep using file backups.

If the browser does not open automatically, start it from a terminal with:

```powershell
.\btc09-windows-amd64.exe app
```

Leave BTC09 open while the chain syncs. The Send screen unlocks when the local
chain has data and at least one peer is connected.

## Run a Full Node

```powershell
btc09 node
```

Mining only starts when `-mine` is present. A normal node still syncs, relays
blocks and transactions, and listens on `:9009` by default.

## Solo Mine

```powershell
btc09 node -mine
```

Use a fixed worker count if you do not want to use every CPU thread:

```powershell
btc09 node -mine -workers 3 -tag yourname
```

When your node finds a block it prints `BLOCK FOUND`. The current block reward
is 50 09C and mined rewards become spendable after 100 blocks.

## Mine in the desktop wallet

Open the BTC09 app, choose **Mine**, select how many CPU threads to use, and
press **Start mining**. This uses the supported open-source miner and pays a
block you find directly to your wallet.

The official software supports local solo, remote solo, and non-custodial
PPLNS mining. Only download BTC09 from the GitHub releases page, and never give
mining software your recovery words, private key, wallet file, remote access,
or Discord token.

## Official PPLNS Mining

The official client uses PPLNS by default without downloading the chain:

```powershell
btc09 mine-pool -pool https://btc09.org -address YOUR_09C_ADDRESS -worker rig-1
```

HTTPS is required by default. Plain HTTP is available only for an explicitly
trusted local test with `-allow-insecure-http`.

The pool fee is 0%. Rewards are split by difficulty-weighted accepted shares
and paid directly in a block coinbase. There is no pool wallet or withdrawal
balance. The client verifies the share window and coinbase proof before mining.

An operator can enable PPLNS on a synced node and place it behind TLS and
connection limits:

```powershell
btc09 node -solo-api 127.0.0.1:9010 -pplns-state C:\secure\btc09\pplns-window.json
```

The coordinator accepts a payout address, not wallet secrets. It creates the
canonical block template and accepts only a nonce for a short-lived job.
Coinbase payouts still need the normal maturity period before they can be
spent.

The desktop wallet has the same open-source miner under **Mine**. It uses your
wallet receive address automatically, shows the current and session-average
hashrate, accepted shares, blocks, and reconnects after temporary endpoint
errors. See `docs/PPLNS-MINING-PROTOCOL.md` to build or operate a compatible
coordinator. Use `-mode solo` and `docs/OPEN-MINING-PROTOCOL.md` when you
specifically want remote solo mining.

## Check Balance

The node prints your reward address at startup. You can check it in the public
explorer:

```text
https://explorer.btc09.org/address/YOUR_ADDRESS
```

Local wallet balance is calculated from your own synced chain. A fresh node can
show a balance later than the public explorer while it catches up.

## Send 09C

09C is a native chain coin, not an Ethereum or Solana token. MetaMask and
Phantom do not support it. Use the BTC09 desktop wallet or command-line node.

List your wallet addresses and local balance:

```powershell
.\btc09-windows-amd64.exe wallet list
```

Create another receiving address:

```powershell
.\btc09-windows-amd64.exe wallet new
```

Send 09C and broadcast through the seed node:

```powershell
.\btc09-windows-amd64.exe send -to THEIR_09C_ADDRESS -amount 100 -seeds seed.btc09.org:9009
```

The default fee is 0.0001 09C. `-seeds` is plural and means peer nodes to
broadcast through. It is not a wallet seed phrase option. Do not paste private
keys, seed phrases, or wallet file contents into the command line.

`send` spends from the wallet file in your data directory. Your local chain must
be synced enough to see spendable coins. Mined rewards become spendable after
100 blocks.

## Run an Explorer

```powershell
btc09 node -explorer :8009
```

Useful API endpoints:

```text
/api/status
/api/supply
/api/circulating_supply
```

`/api/circulating_supply` returns only the plain circulating supply number for
listing and audit integrations.

## Upgrade

1. Stop your running `btc09` process.
2. Replace the old binary with the new release binary.
3. Start it again with the same command.

Example:

```powershell
btc09 version
btc09 node -mine -workers 3 -tag yourname
```

v0.1.10 and later improve fresh-node sync and log wallet balance changes as soon
as the local chain sees them.
