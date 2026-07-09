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
Bitcoin 09 (09C) reference node v0.1.19
```

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

## Pool Mine

Public pool:

https://bitcoin09.tutuit.xyz

Use the pool's current worker instructions from the pool page.

The pool currently recommends NTMminer for pool mining. NTMminer is a
third-party closed-source binary miner, not the official 09C wallet or node. It
only needs your 09C payout address. Do not give any pool miner a seed phrase,
private key, wallet file, remote access, or Discord token.

NTMminer supports GPU mining for some other algorithms, but its `btc09`
algorithm is CPU in the current public release. There is no official 09C GPU
miner right now. 09C does not police hardware at consensus level: if a miner
finds valid blocks, those blocks are valid.

Pool miner count means payout addresses connected to that public pool, not
guaranteed unique people and not solo miners.

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
Phantom do not support it. Use the `btc09` wallet/node.

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
