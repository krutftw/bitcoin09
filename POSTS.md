# Launch posts

## Bitcointalk / forum post

Title:

```text
[ANN] Bitcoin 09 (09C) - CPU mining like it's 2009, no premine
```

Body:

```text
I made Bitcoin 09 because I wanted to bring back the part of Bitcoin most of
us missed: mining on a normal computer when the coins were worth nothing.

09C is Bitcoin with one change: proof of work is Argon2id instead of
SHA-256. The goal is to keep mining on ordinary CPUs instead of ASICs.

Kept from Bitcoin:
- 21 million cap
- 50 coin block reward
- halving every 210,000 blocks
- 10 minute block target
- UTXO model
- no premine
- no allocation
- unspendable genesis reward

Genesis:
ba685f741a04ddad03d37500ff354ce3887e64dd9cb6154ae236952792e90c3f

Genesis message:
the coin that you can mine like it's 2009

Seed:
seed.btc09.org:9009

Explorer:
https://explorer.btc09.org

Source and releases:
https://github.com/krutftw/bitcoin09

Discord:
https://discord.gg/fUuGzwRTzP

Run:
git clone https://github.com/krutftw/bitcoin09
cd bitcoin09
go build ./cmd/btc09
./btc09 node -mine -seeds seed.btc09.org:9009

If you downloaded an early build, upgrade to the latest release. Older clients
can sit on stale forks from before the retarget and sync fixes.

09C has no price. It might never have one. Mine it if you want to join a
fair-launch proof-of-work chain from the start.
```

## Short post

```text
I launched Bitcoin 09 (09C): Bitcoin economics, one change: Argon2id CPU
proof of work so people can mine on normal computers again.

21M cap, halvings, 10 min blocks, no premine, burned genesis.

Seed: seed.btc09.org:9009
Explorer: https://explorer.btc09.org
Repo: https://github.com/krutftw/bitcoin09
Discord: https://discord.gg/fUuGzwRTzP

Mine it if you want to do the 2009 part again.
```

## Technical post

```text
Bitcoin 09 is a fair-launch PoW chain.

Consensus:
- UTXO ledger
- 21M cap
- 50 coin subsidy, halving every 210,000 blocks
- 10 minute target
- difficulty retarget every 2016 blocks, 4x clamp
- Ed25519 signatures
- 1 MB blocks
- heaviest cumulative work wins
- Argon2id PoW, 64 MiB per attempt

Genesis id:
ba685f741a04ddad03d37500ff354ce3887e64dd9cb6154ae236952792e90c3f

Source:
https://github.com/krutftw/bitcoin09

Discord:
https://discord.gg/fUuGzwRTzP
```
