# Bitcoin 09 (09C)

![Bitcoin 09 logo](docs/assets/bitcoin09-ai-logo-512.png)

Fair-launch CPU proof of work: 21M cap, 10 minute blocks, no premine,
Argon2id mining.

Bitcoin 09 keeps the Bitcoin shape and changes one thing: proof of work is
Argon2id instead of SHA-256. Normal CPUs can mine it, and the 64 MiB memory
cost is meant to reduce the easy ASIC/GPU advantage that took over Bitcoin.

```text
site:     https://btc09.org
explorer: https://explorer.btc09.org
discord:  https://discord.gg/fUuGzwRTzP
release:  https://github.com/krutftw/bitcoin09/releases/latest
```

| | Bitcoin | Bitcoin 09 |
|---|---|---|
| Supply cap | 21,000,000 | 21,000,000 |
| Block reward | 50, halves every 210,000 blocks | 50, halves every 210,000 blocks |
| Block time | 10 minutes | 10 minutes |
| Premine | none | none |
| Genesis reward | unspendable | unspendable |
| Proof of work | SHA-256d (ASICs) | Argon2id 64 MiB (CPU-accessible) |
| Signatures | ECDSA/Schnorr | Ed25519 |

## Read this first

09C is worth nothing. Bitcoin was worth nothing in 2009 too, that's the
whole idea here. No premine, no company, no allocation. The genesis reward is
burned to an address nobody can spend from, so every coin that will ever
exist gets mined publicly starting at block 1. Mine it because it's fun and
you missed 2009. Don't put in money you care about, and don't expect any
back.

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
./btc09 send -to ADDRESS -amount 1.5 -seeds seed.btc09.org:9009
```

`-seeds` is plural and means peer nodes to broadcast through. It is not a wallet
seed phrase option. Do not paste private keys, seed phrases, or wallet file
contents into the command line. `send` spends from the wallet file in your data
directory.

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

## Mining policy

09C is permissionless proof of work. The official node miner is CPU mining,
and the current public third-party `btc09` miner is listed as CPU-only, but
the chain does not try to detect or ban hardware. A valid block is valid
work.

If a public GPU, FPGA or heavily optimized miner appears, it should be shared
and benchmarked like any other miner. If the community later wants a stronger
CPU-biased PoW, that requires a coordinated hard fork, not pool-side rules.

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

Default bootstrap seeds are built into the client:

```text
seed.btc09.org:9009
178.128.105.41:9009
103.80.18.140:9009
108.190.240.138:9009
```

Live block explorer:

```text
https://explorer.btc09.org
```

Supply/status APIs:

```text
https://explorer.btc09.org/api/status
https://explorer.btc09.org/api/supply
https://explorer.btc09.org/api/circulating_supply
```

`/api/circulating_supply` returns the plain circulating supply number. It
excludes the burned genesis reward.
`/api/status` also includes the current retarget window: target seconds,
observed average block time, blocks to the next retarget, and estimated next
difficulty if the window keeps the same average.

Discord:

```text
https://discord.gg/fUuGzwRTzP
```

Any node can serve its own explorer with `-explorer :8009`.

## Markets

There is no official 09C price. Early trading is community OTC only.

Discord has `#💱-otc-trading` for quick negotiation and an optional OTC escrow
bot for small trades. The bot can hold the seller's 09C, verify the order's
deposit address, and release after both sides confirm. Payment in USDT/BTC/fiat
still happens directly between buyer and seller, so disputes still need human
review.

The public OTC board at https://btc09.org/markets.html lets
users draft buy offers, sell offers, and completed-trade references on the
website, copy a Discord post, and open a prefilled GitHub issue for the public
record. It is not an exchange, it does not set an official price, and there are
no price promises.

The hosted board reads the sanitized Discord escrow feed from the BTC09 server.
Public offer and completed-trade records are backed by GitHub issues and a
VPS-generated `market-data.json` snapshot, so normal page views do not need to
hit the GitHub API.

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
right now. If a third-party GPU miner appears later, that is still
permissionless mining. Treat any closed-source pool miner carefully: use only
a payout address and never give it wallet secrets.

## Tests

```
go test ./...
```

Covers the emission schedule and 21M cap, rejection of inflated coinbases,
signature checks (wrong key can't spend), double spend rejection, difficulty
retargeting, reorgs, and the pinned genesis.

## Status

v0.1.18 is current. The node, miner, wallet, P2P sync, built-in seeds, block
explorer and supply APIs are live.

Network:

```text
site:     https://btc09.org
seeds:    seed.btc09.org:9009, 178.128.105.41:9009, 103.80.18.140:9009, 108.190.240.138:9009
explorer: https://explorer.btc09.org
release:  https://github.com/krutftw/bitcoin09/releases/latest
discord:  https://discord.gg/fUuGzwRTzP
```

If you downloaded an early build, upgrade to the latest release. Old clients
from before the fork-sync and retarget fixes can get stuck on stale forks.

Stuff I want next, PRs welcome: peer banning and DoS hardening, headers-first
sync, DNS seeds, miner/peer stats in the explorer and a friendlier miner UI.

## License

MIT, same as Bitcoin.
