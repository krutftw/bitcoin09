# Bitcoin 09 launch

I made Bitcoin 09 because I missed the thing none of us can go back to:
mining Bitcoin on a normal computer when it was worth nothing.

09C is Bitcoin with one change. The proof of work is Argon2id instead of
SHA-256, so normal CPUs can mine it and ASICs do not get the same kind of
advantage they get on Bitcoin.

Everything else is kept close to Bitcoin:

- 21 million coin cap
- 50 coin block reward
- halving every 210,000 blocks
- 10 minute block target
- no premine
- no allocation
- unspendable genesis reward
- UTXO transactions
- open source from the start

Genesis:

```text
ba685f741a04ddad03d37500ff354ce3887e64dd9cb6154ae236952792e90c3f
```

Genesis message:

```text
the coin that you can mine like it's 2009
```

Bootstrap seeds:

```text
seed.btc09.org:9009
178.128.52.20:9009
178.128.105.41:9009
103.80.18.140:9009
108.190.240.138:9009
```

Website:

```text
https://btc09.org
```

Explorer:

```text
https://explorer.btc09.org
```

Discord:

```text
https://discord.gg/fUuGzwRTzP
```

OTC board:

```text
https://btc09.org/markets.html
```

Build from source:

```bash
git clone https://github.com/krutftw/bitcoin09
cd bitcoin09
go build ./cmd/btc09
./btc09 node -mine -seeds seed.btc09.org:9009,178.128.52.20:9009,178.128.105.41:9009,103.80.18.140:9009,108.190.240.138:9009
```

Or download the latest release, currently v0.1.25:

```text
https://github.com/krutftw/bitcoin09/releases/latest
```

If you downloaded an early build, upgrade. Older clients can sit on stale
forks from before the retarget and sync fixes.

This has no price. It may never have a price. Mine it if you want to be early
to a fair-launch proof-of-work chain and you like the idea of doing the 2009 part again.
