# Bitcoin 09 (09C)

The coin that you can mine like it's 2009.

This is Bitcoin, same idea top to bottom, changed in one place: the proof of
work is Argon2id (memory-hard) instead of SHA-256. That means normal CPUs do
the mining and ASICs/GPUs get no real advantage. Everything else follows
Bitcoin.

| | Bitcoin | Bitcoin 09 |
|---|---|---|
| Supply cap | 21,000,000 | 21,000,000 |
| Block reward | 50, halves every 210,000 blocks | 50, halves every 210,000 blocks |
| Block time | 10 minutes | 10 minutes |
| Premine | none | none |
| Genesis reward | unspendable | unspendable |
| Proof of work | SHA-256d (ASICs) | Argon2id 64 MiB (CPUs) |
| Signatures | ECDSA/Schnorr | Ed25519 |

## Read this first

09C is worth nothing. Bitcoin was worth nothing in 2009 too, that's the
whole idea here. No premine, no company, no allocation. The genesis reward is
burned to an address nobody can spend from, so every coin that will ever
exist gets mined by someone's CPU starting at block 1. Mine it because it's
fun and you missed 2009. Don't put in money you care about, and don't expect
any back.

## Quick start

You need [Go 1.25+](https://go.dev/dl/).

```
git clone https://github.com/krutftw/bitcoin09
cd bitcoin09
go build ./cmd/btc09
```

Mine with all your cores:

```
./btc09 node -mine
```

Your reward address prints at startup (wallet auto-created in `~/.btc09`).

For the shortest setup path, read [QUICKSTART.md](QUICKSTART.md).

Run a node without mining:

```
./btc09 node
```

Mining only starts when `-mine` is present. A non-mining node still syncs,
relays blocks/transactions, listens on `:9009` by default, and can serve its
own explorer with `-explorer :8009`.

## Quick start, Windows release

1. Download `btc09-windows-amd64.exe` from the latest release.
2. Open PowerShell in the download folder.
3. Run:

```powershell
.\btc09-windows-amd64.exe node -mine
```

Leave it open. When your computer finds a block, it prints `BLOCK FOUND`.
Your wallet is created automatically under your user folder.

Wallet stuff:

```
./btc09 wallet list
./btc09 wallet new
./btc09 send -to ADDRESS -amount 1.5 -seeds 82.22.32.82:9009
```

Balance notes:

- The wallet balance is calculated from your local node's synced chain, so a
  fresh node may show it later than the public explorer while it catches up.
- v0.1.10 and later sync fresh nodes in larger bounded batches and log balance
  changes as soon as the local chain sees them.
- The running node still prints a regular status line every 30 seconds.
- Coinbase mining rewards are spendable after 100 blocks, same as Bitcoin.
- Mainnet nodes check GitHub for a newer release at startup and only print a
  notice if one exists. Nothing auto-installs. Use `-no-update-check` to skip
  that check.

Local sandbox with instant blocks:

```
./btc09 node -mine -network regtest -datadir /tmp/sandbox
```

## Why change the proof of work

Bitcoin mining stopped being for normal computers around 2013 when ASICs
took over. Today a laptop has a one in millions share against warehouse
hardware.

Argon2id needs 64 MiB of memory per hash attempt. Memory bandwidth is the
bottleneck, not raw logic speed, so a GPU's thousands of cores sit starved
and an ASIC would basically be a stack of the same DRAM your laptop already
has. Monero's RandomX has used the same reasoning for years and CPU mining
is still viable there.

Nothing stays ASIC-proof forever, but memory-hard keeps the gap between a
laptop and custom hardware small. That's what makes the 2009 experience
possible again.

## Consensus rules, short version

- 88 byte headers, block ids are SHA-256d, PoW check is Argon2id(header) <= target
- difficulty retargets every 2016 blocks, same as Bitcoin, max 4x move per step
- UTXO model, Ed25519 signatures, base58check addresses (version 0x09)
- coinbase pays subsidy + fees, spendable after 100 blocks
- heaviest total work wins, reorgs replay the UTXO set
- 1 MB max block

Mainnet genesis is pinned by a test:
`ba685f741a04ddad03d37500ff354ce3887e64dd9cb6154ae236952792e90c3f`
with the message "the coin that you can mine like it's 2009", nonce 20214.

The first seed node is built into the client:

```text
82.22.32.82:9009
```

Live block explorer:

```text
http://82.22.32.82:8009
```

Supply/status APIs:

```text
http://82.22.32.82:8009/api/status
http://82.22.32.82:8009/api/supply
http://82.22.32.82:8009/api/circulating_supply
```

`/api/circulating_supply` returns the plain circulating supply number. It
excludes the burned genesis reward.

Discord:

```text
https://discord.gg/fUuGzwRTzP
```

Any node can serve its own explorer with `-explorer :8009`.

## Markets

There is no official 09C price. Early trading is community OTC only.

Discord has `#💱-otc-trading` for users who want to post buy/sell offers or
factual completed-trade references. Staff does not provide official escrow and
there are no price promises.

## Tests

```
go test ./...
```

Covers the emission schedule and 21M cap, rejection of inflated coinbases,
signature checks (wrong key can't spend), double spend rejection, difficulty
retargeting, reorgs, and the pinned genesis.

## Status

v0.1.11 is current. The node, miner, wallet, P2P sync, built-in seed, block
explorer and supply APIs are live.

Network:

```text
seed:     82.22.32.82:9009
explorer: http://82.22.32.82:8009
release:  https://github.com/krutftw/bitcoin09/releases/latest
discord:  https://discord.gg/fUuGzwRTzP
```

If you downloaded an early build, upgrade to the latest release. Old clients
from before the fork-sync and retarget fixes can get stuck on stale forks.

Stuff I want next, PRs welcome: a second public seed, peer banning and DoS
hardening, headers-first sync, DNS seeds, miner/peer stats in the explorer and
a friendlier miner UI.

## License

MIT, same as Bitcoin.
