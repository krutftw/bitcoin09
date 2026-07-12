# Bitcoin 09 (09C) exchange listing spec

Current, verifiable reference for exchange review. Check the live chain state
and current release again when submitting.

## Listing route

The first target is SafeTrade. Its official public route is the
[SafeTrade support request form](https://support.safetrade.com/hc/en-us/requests/new).
The trading API key on a user account is for that user's orders and balances;
it does not create a coin listing request.

Other small exchanges should be approached only after confirming that their
current listing route is active and that BTC09 can meet their technical and
commercial requirements. Do not spam several exchanges with stale form links.

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
- **Difficulty retarget:** every 2,016 blocks, limited to a 4x change
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

At the 12 July 2026 review, mainnet was above height 7,370 with more than
368,000 09C circulating. These numbers change with every block.

## Network

- **Machine network ID:** `btc09-mainnet`
- **Default P2P port:** `9009/tcp`
- **Address version:** `0x09`; current addresses start with `4k`
- **Seed:** `seed.btc09.org:9009`
- **Additional public peers:** `178.128.105.41:9009`,
  `103.80.18.140:9009`, `108.190.240.138:9009`
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
and Windows amd64 binaries. `SHA256SUMS.txt` is published beside them.

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
  ANN, multiple public peers, and a working community OTC path.

There are no price, volume, or return promises. A listing would give miners and
new users a safer public transfer path than informal Discord settlement.

## Contact

Use the official listing ticket for private contact details. Public technical
questions can be opened at https://github.com/krutftw/bitcoin09/issues without
including secrets, wallet data, or exchange account information.
