# Bitcoin 09 (09C)

![Bitcoin 09 logo](docs/assets/bitcoin09-ai-logo-512.png)

The coin that you can mine like it's 2009.

This is Bitcoin, same idea top to bottom, changed in one place: the proof of
work is Argon2id (memory-hard) instead of SHA-256. Normal CPUs can mine it,
and the 64 MiB memory cost is meant to reduce the easy ASIC/GPU advantage
that took over Bitcoin. Everything else follows Bitcoin.

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

09C is a native chain coin, not an Ethereum or Solana token. MetaMask,
Phantom, and token-contract wallets do not support it. Use the `btc09` wallet
for now.

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
bottleneck, not raw logic speed, so GPUs and ASICs do not get the same clean
advantage they get on SHA-256d. Monero's RandomX has used the same broad
reasoning for years and CPU mining is still viable there.

Nothing stays ASIC-proof or GPU-proof forever. The aim is simpler: make the
gap between normal CPUs and custom hardware smaller than it is on Bitcoin.
That's what makes the 2009 experience possible again.

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

Discord has `#💱-otc-trading` for quick negotiation. The public OTC board at
https://krutftw.github.io/bitcoin09/markets.html lets users draft buy offers,
sell offers, and completed-trade references on the website, copy a Discord post,
and open a prefilled GitHub issue for the public record. Staff does not provide
official escrow, does not process payments, and there are no price promises.

The board is backed by public GitHub issues and a static
`docs/market-data.json` snapshot generated by GitHub Actions, so normal page
views do not need to hit the GitHub API.

## Brand

Logo and brand files are in [BRAND.md](BRAND.md). Use these instead of
random coin art so Bitcoin 09 looks consistent across Discord, pool pages,
forum posts and listings.

## Third-party pool miners

The official Bitcoin 09 software is `btc09`: reference node, wallet, solo
miner and explorer.

The public pool currently recommends NTMminer for pool mining. NTMminer is a
third-party closed-source binary miner that added `-a btc09` support in its
v1.13.0 release. It only needs a payout address. Do not give any pool miner a
seed phrase, private key, wallet file, remote access or Discord token.

NTMminer has NVIDIA GPU support for some other algorithms, but its public
`btc09` backend is listed as CPU-only. There is no official 09C GPU miner
right now. If a third-party GPU miner appears later, treat it like any other
closed-source pool miner: use only a payout address and never give it wallet
secrets.

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
