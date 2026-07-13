# Bitcoin 09 v0.1.28

This release adds the official non-custodial PPLNS mining path. It changes
mining coordination and the wallet interface, not consensus, supply, proof of
work, transaction rules, or the P2P protocol.

## Official PPLNS mining

- The desktop wallet and `mine-pool` command use PPLNS by default.
- Every accepted share is written to a crash-durable rolling window before the
  coordinator acknowledges it.
- Shares are weighted by the expected work represented by their accepted
  target, so a difficulty change does not make unlike shares count as equal.
- A winning block pays the frozen prior window directly from its coinbase.
  There is no custody wallet, internal balance, or withdrawal step.
- The official pool fee is 0%.
- The client verifies the committed share window, payout weights, exact
  coinbase transaction, and coinbase Merkle proof before hashing.
- Accepted submission receipts are idempotent during a coordinator run, so a
  dropped HTTP response can be retried safely.
- The miner binds each receipt to the submitted height and proof-of-work hash,
  then fetches fresh verified work after every accepted share.

The public protocol uses these exact routes:

```text
POST /api/v2/pool/work
POST /api/v2/pool/submit
GET  /api/v2/pool/status
```

Open Mining Protocol v1 remains available for explicit remote solo mining with
`mine-pool -mode solo`.

## Wallet and operator changes

- The Mine screen shows accepted shares separately from blocks.
- Help reports include pool mode, fee, share count, and block count without
  including the payout address or worker label.
- A node can enable PPLNS with a process-exclusive state file, bounded share
  window, bounded address count, and bounded share-target multiplier.
- The nginx deployment exposes only the two exact v1 POST routes and the three
  exact v2 routes, with separate rate limits and a loopback upstream.
- Pool status exposes the public payout ledger and weights, but not source IPs
  or worker labels.

## Important limits

PPLNS reduces solo variance but does not promise a payout, price, or return.
Coinbase rewards need 100 confirmations before they can be spent. A block that
is orphaned during a reorganization does not pay. Payout addresses and accepted
shares are public because miners must be able to audit the next allocation.

## Upgrade

Replace the executable with the matching v0.1.28 file for your platform and
verify it against `SHA256SUMS`. Existing Wallet V1 and Wallet V2 files remain
compatible. Node operators enabling PPLNS must keep the state file on durable
local storage and must not run two coordinators against the same file.
