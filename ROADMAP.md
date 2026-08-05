# Roadmap

This file tracks where Bitcoin 09 development is headed. It lists work in
rough priority order, not a schedule. Nothing here is a promise or a date.
Consensus, proof of work, supply, rewards, and addresses are not changing;
anything that ever required a consensus change would need a coordinated hard
fork announced well in advance.

## Recently shipped

- Per-block ASERT difficulty (two-hour half-life), active since height 12,096
  (v0.1.34). See [the ASERT design note](docs/plans/2026-07-17-asert-difficulty-upgrade-design.md).
- v0.1.35 wallet apps for Windows, macOS, Linux, and Android, with checksums
  and provenance on every release.
- Non-custodial 0%-fee PPLNS pool mining. The coordinator never holds keys or
  balances; payouts go straight into the coinbase.
- Versioned read-only explorer API (`/api/v1/tip`, blocks, transactions,
  address outputs) for exchange and service integration.
- Host-level network guard on the production nodes after the July packet
  floods.

## Node and network

- Peer scoring and banning, so misbehaving peers get dropped instead of
  retried.
- Headers-first sync to speed up initial block download.
- DNS seeds beyond the current `seed.btc09.org`.
- More independent public peers. Running a reachable node on port 9009 is the
  single most useful thing a supporter can do today.

## Explorer and mining

- Miner and peer statistics in the explorer.
- A friendlier miner UI in the desktop wallet.
- Continued review of community miners. Third-party miner releases are
  reviewed before they are linked anywhere; two reviews to date found bugs
  that blocked a recommendation, and re-reviews happen after fixes.

## Wallet

- Wallet v2 recovery format, so a single backup phrase can restore funds
  without the original wallet file. See
  [the recovery design](docs/plans/2026-07-13-wallet-v2-recovery-design.md).

## Markets and listings

- A first public order book. Every application, requirement, and published
  paid route is tracked openly at
  [btc09.org/exchanges.html](https://btc09.org/exchanges.html); statuses come
  from tickets and written replies, never assumptions.
- Free price-aggregator pages (the kind that accept coins before a market
  exists), then the market-gated ones once a real order book is live.
- No wash trading, no paid fake volume, and no payment to any venue before
  written integration, custody, liquidity, delisting, and refund terms.

## Not planned

- No premine reversal, treasury, or team allocation. There is nothing to
  allocate.
- No token, contract, or bridge versions of 09C on other chains.
- No changes to the 21M cap, halving schedule, or Argon2id parameters.

PRs are welcome for anything above. If you want to work on one of these, open
an issue first so effort does not get duplicated.
