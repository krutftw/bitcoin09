# Bitcoin 09 (09C) exchange listing spec

Current, verifiable reference for exchange review. Check the live chain state
and current release again when submitting.

## Listing route

Use only an exchange's official listing form, helpdesk, or published listing
address. The public tracker at https://btc09.org/exchanges.html is the canonical
status page, backed by the machine-readable `docs/exchanges.json` file. The
trading API key on an exchange account is for that user's orders and balances;
it does not create a coin listing request.

## Outreach status

Reviewed 1 August 2026. Prices and requirements can change, so obtain written
terms from the exchange before paying or supplying liquidity.

| Status | Venues |
| --- | --- |
| Submitted / awaiting reply | SafeTrade, CoinEx, FreiExchange, MEXC, Bitget, BitMart, LBank, Biconomy, Toobit, AscendEX, Deepcoin, KCEX, Phemex, Bitfinex |
| Written terms requested | XT |
| Published paid routes | NonKYC, XeggeX |
| Requirements not currently met | Bitrue, BingX, Gate, Bybit, Kraken, Coinbase, Crypto.com, KuCoin, OKX, HTX |
| Engineering prerequisite | BasicSwap, DEX Screener |
| No verified public route | TradeOgre |

Applications under review are not listings, approvals, or partnerships. Do not
send duplicate applications or routine follow-ups before the venue's own review
window has passed.

Official routes checked:

- https://support.safetrade.com/hc/en-us/requests/new
- https://www.coinex.com/en/apply/create
- https://www.coinex.com/en/help/sections/articles/900004236303
- https://helpdesk.freiexchange.com/open.php
- https://helpdesk.freiexchange.com/kb/faq.php?id=3
- https://phemex.com/announcements/listing-application
- https://blog.bitfinex.com/announcements/introducing-the-new-token-listing-form/
- https://www.kucoin.com/support/26125293810713
- https://nonkyc.io/listing
- https://xeggex.com/listing
- https://docs.dexscreener.com/token-listing
- https://academy.particl.io/en/latest/basicswap-guides/basicswapguides_apply.html

## Funding decision

An exchange listing can improve access and price discovery, but paying a venue
does not create durable value. Do not buy 09C from yourself, wash trade, or pay
for fake volume.

Use project funds in this order:

1. keep the node, explorer, seed, wallet downloads, and support path reliable;
2. maintain reproducible signed releases and native-chain integration tests;
3. submit free applications and answer technical reviews;
4. consider a paid listing only after receiving written integration, custody,
   liquidity, withdrawal, delisting, and refund terms;
5. disclose who supplies any launch liquidity and where its 09C came from.

Bitcoin 09 has no premine or project treasury. Any 09C used for liquidity must
therefore be mined or acquired through the same public paths available to other
participants. A commercial commitment needs a separate, explicit budget; it is
not part of routine exchange outreach.

## Coin summary

Bitcoin 09 (`09C`) is a fair-launch, CPU-accessible cryptocurrency with a
Bitcoin-style UTXO ledger and Argon2id proof of work. There was no premine, ICO,
developer allocation, or treasury allocation. The genesis reward is
unspendable, so every circulating coin was publicly mined after genesis.

- **Ticker:** 09C
- **Name:** Bitcoin 09
- **Type:** native UTXO chain, not an EVM or Solana token
- **Proof of work:** Argon2id, 64 MiB memory cost
- **Implementation:** clean-room Go reference node
- **Website:** https://btc09.org
- **Source:** https://github.com/krutftw/bitcoin09 (MIT)
- **Explorer:** https://explorer.btc09.org
- **Integration guide:** https://github.com/krutftw/bitcoin09/blob/master/docs/EXCHANGE-INTEGRATION.md
- **Latest release:** https://github.com/krutftw/bitcoin09/releases/latest
- **Bitcointalk ANN:** https://bitcointalk.org/index.php?topic=5587640.0
- **Discord:** https://discord.gg/fUuGzwRTzP

## Monetary policy

- **Maximum nominal supply:** 21,000,000 09C
- **Initial block subsidy:** 50 09C
- **Halving interval:** 210,000 blocks
- **Target block time:** 600 seconds
- **Difficulty adjustment:** per-block ASERT from height 12,096 (two-hour half-life); legacy 2,016-block windows before activation
- **Coinbase maturity:** 100 blocks
- **Premine / ICO / team allocation:** none
- **Genesis reward:** burned and excluded from circulating supply

## Live chain state

Use the chain APIs rather than a hard-coded application value:

```bash
curl -fsS https://explorer.btc09.org/api/v1/tip
curl -fsS https://explorer.btc09.org/api/status
curl -fsS https://explorer.btc09.org/api/circulating_supply
```

Do not copy a height, difficulty, hashrate, peer count, or circulating supply
from this document into an application. Read the live API immediately before
submitting because these values change with the chain.

## Network

- **Machine network ID:** `btc09-mainnet`
- **Default P2P port:** `9009/tcp`
- **Address version:** `0x09`; current addresses start with `4k`
- **Seed:** `seed.btc09.org:9009`
- **Additional public peers:** `178.128.52.20:9009`,
  `178.128.105.41:9009`, `103.80.18.140:9009`, `108.190.240.138:9009`
- **Mainnet genesis:**
  `ba685f741a04ddad03d37500ff354ce3887e64dd9cb6154ae236952792e90c3f`

Versioned read-only integration endpoints:

```text
GET /api/v1/tip
GET /api/v1/block/{hash}
GET /api/v1/transaction/{txid}
GET /api/v1/address/{address}/outputs?expected_tip_hash={hash}&expected_tip_height={height}
```

## Software and integration

The latest release includes checksummed Linux amd64/arm64, macOS amd64/arm64,
and Windows amd64 binaries. `SHA256SUMS` is published beside them.

The operator guide documents the complete flow:

1. verify and pin the release;
2. run a non-mining exchange node;
3. allocate unique JSON-returned deposit addresses;
4. scan outputs against an exact tip;
5. handle confirmations and reorgs;
6. prepare, independently inspect, reserve, and broadcast withdrawals;
7. reconcile and recover after incidents.

Read-only public check:

```bash
python3 tools/exchange/btc09_exchange_smoke.py \
  --base-url https://explorer.btc09.org
```

Recommended starting deposit policy is 100 confirmations while the network is
young. Each exchange should set and review its own risk policy.

## Why consider 09C

- Fair-launch supply is verifiable from genesis and the emission code.
- The node, wallet, P2P network, explorer, miner, and transaction flow are live.
- CPU-accessible Argon2id mining gives ordinary hardware a direct way to join.
- The implementation and integration tests are small enough to audit in full.
- The project has public releases, checksums, an active Discord, a Bitcointalk
  ANN, multiple public peers, and public wallet and transaction tooling.

There are no price, volume, or return promises. A listing would give miners and
new users a safer public transfer path than informal Discord settlement.

## Contact

Use the official listing ticket for private contact details. Public technical
questions can be opened at https://github.com/krutftw/bitcoin09/issues without
including secrets, wallet data, or exchange account information.
