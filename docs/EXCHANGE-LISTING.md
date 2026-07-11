# Bitcoin 09 (09C) — Exchange Listing Spec

Ready-to-paste reference for exchange listing applications. Everything an
exchange needs to evaluate and integrate 09C is here. Keep this in sync with
the live chain state when submitting.

## Submit to

Free or low-cost listings that accept CPU / fair-launch coins:

| Exchange | URL | Notes |
|----------|-----|-------|
| TradeOgre | https://tradeogre.com/listing/add | The standard venue for CPU-mined coins. Simple form. |
| Xeggex | https://xeggex.com/listing/request | Common for new CPU coins. |
| NonKyc.io | https://nonkyc.io | Privacy-friendly, lists fair-launch coins. |
| MEXC | https://www.mexc.com/ | Longer shot, but lists small market-cap coins via "Space". |

---

## Coin summary (copy-paste)

**Bitcoin 09 (09C)** is a fair-launch, CPU-mineable cryptocurrency that keeps
Bitcoin's economics and changes one thing: the proof of work is **Argon2id**
(64 MiB) instead of SHA-256, so ordinary CPUs can mine it and ASICs do not get
the same kind of advantage. There was no premine, no ICO, no developer
allocation, and the genesis block reward is burned — every coin that exists was
or will be mined by someone's CPU.

- **Ticker:** 09C
- **Name:** Bitcoin 09
- **Algorithm:** Argon2id (64 MiB, memory-hard)
- **Type:** UTXO, Bitcoin-style (not an EVM token; no smart contracts)
- **Implementation:** Clean-room Go reference node (not a Bitcoin Core fork)
- **Website:** https://btc09.org
- **Source code:** https://github.com/krutftw/bitcoin09 (open source, MIT)
- **Explorer:** https://explorer.btc09.org
- **Bitcointalk ANN:** https://bitcointalk.org/index.php?topic=5587640.0
- **Discord:** https://discord.gg/fUuGzwRTzP
- **Public mining pool:** https://www.ntmminer.com/btc09 (third-party)

## Monetary policy

- **Max supply:** 21,000,000 09C (hard cap, same as Bitcoin)
- **Block reward:** 50 09C (halving every 210,000 blocks)
- **Block target:** 10 minutes
- **Difficulty retarget:** every 2,016 blocks, Bitcoin-style (max 4x per adjustment)
- **Genesis reward:** burned (unspendable), so the effective circulating cap is slightly under 21M
- **Premine:** none
- **Allocation:** none — no developer, team, or treasury allocation

## Current chain state

Fill these in from the live explorer before submitting:

```
Chain height:        https://explorer.btc09.org   (top-right)
Circulating supply:  curl https://explorer.btc09.org/api/circulating_supply
Difficulty:          https://btc09.org            (Network Now panel)
Network hashrate:    https://btc09.org            (Network Now panel)
Circulating coins:   ~350,000 09C as of July 2026
```

## Network / integration specs

- **Seed nodes:**
  - `seed.btc09.org:9009`
  - `178.128.105.41:9009`
  - `103.80.18.140:9009`
  - `108.190.240.138:9009`
- **Default P2P port:** 9009
- **Address version byte:** 0x09 (addresses start with `4k...`)
- **Explorer JSON API:**
  - Tip: `https://explorer.btc09.org/api/status`
  - Supply: `https://explorer.btc09.org/api/circulating_supply`

## Wallet / node software

Prebuilt binaries for exchange integration:

```
Linux amd64:   https://github.com/krutftw/bitcoin09/releases/latest (btc09-linux-amd64)
Linux arm64:   https://github.com/krutftw/bitcoin09/releases/latest (btc09-linux-arm64)
macOS arm64:   https://github.com/krutftw/bitcoin09/releases/latest (btc09-macos-apple)
macOS intel:   https://github.com/krutftw/bitcoin09/releases/latest (btc09-macos-intel)
Windows amd64: https://github.com/krutftw/bitcoin09/releases/latest (btc09-windows-amd64.exe)
```

All binaries are checksummed; verify with the `SHA256SUMS.txt` alongside each
release. The reference node supports wallet commands, transaction inspection,
and broadcast — enough for an exchange to manage deposits/withdrawals without a
third-party library.

## Why list 09C

- Genuinely fair launch — no premine, no allocation, verifiable from genesis.
- CPU-mineable, which draws an active, distributed mining community (not hashrate rentals).
- Real, working infrastructure from day one: live explorer, seed nodes, a public pool, Discord OTC escrow.
- Open source from genesis, clean-room Go implementation — auditable in full.
- The "Bitcoin, but mineable again" narrative has proven audience demand in this category.

## Contact

Listing inquiries: the official Discord (https://discord.gg/fUuGzwRTzP) or the
Bitcointalk ANN thread.
