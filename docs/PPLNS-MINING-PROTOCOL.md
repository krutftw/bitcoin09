# PPLNS Mining Protocol v2

BTC09 PPLNS v2 is a non-custodial pooled-mining protocol. Miners submit
lower-difficulty shares, and a rolling window determines the outputs in the
next block coinbase. The pool fee is fixed at 0%. The coordinator never holds
a pool wallet or a miner balance.

Open Mining Protocol v1 remains available for remote solo mining. V2 uses
separate routes and does not change v1 clients.

## Payout rule

- The default window contains the last 256 accepted shares.
- At most 64 distinct payout addresses can be present in that window.
- Each share is weighted by `2^256 / (share_target + 1)`. This keeps accounting
  fair when network difficulty changes.
- Every available coinbase unit, including transaction fees, is allocated to
  window addresses. Integer leftovers use largest-remainder rounding, with the
  canonical address string as the final tie-breaker.
- Outputs are ordered by canonical address string.
- A block template uses a frozen copy of the window. A share found while
  mining that template enters the following window, including the
  network-winning share.
- If the window is empty, the first template pays its requesting address. The
  first accepted share then starts the shared window.
- Normal coinbase maturity still applies. A direct payout is not spendable
  until the network's coinbase-maturity rule is satisfied.
- An orphaned block does not pay anyone. Its transaction outputs disappear
  with the block under normal consensus and reorganization rules.

There is no operator output and no off-chain withdrawal step.

## Routes

The public interface has three exact routes:

```text
POST /api/v2/pool/work
POST /api/v2/pool/submit
GET  /api/v2/pool/status
```

Unknown fields, duplicate JSON keys, trailing JSON values, path aliases, and
oversized requests are rejected. Production deployments should expose these
routes through HTTPS while the node listens on loopback.

### Request work

```json
{
  "address": "09C payout address",
  "worker": "home-pc"
}
```

`worker` is optional and is not stored in the payout ledger. The response
contains:

- the 88-byte header with a zero nonce;
- separate network and share targets;
- Argon2id parameters and expiry time;
- the frozen PPLNS window and its SHA256d state hash;
- difficulty-weighted payout totals;
- the exact coinbase transaction; and
- a Merkle branch proving that coinbase is committed by the header.

The state hash is also embedded in the coinbase tag. A client must verify the
window, weights, exact-sum payout outputs, coinbase commitment, Merkle branch,
network identity, targets, and Argon2id parameters before hashing.

### Submit a share

```json
{
  "job_id": "32 lowercase hex characters",
  "nonce": 12345
}
```

The coordinator reconstructs its own block and changes only the nonce. A hash
above the share target is rejected. A qualifying share is written to the
durable window before the server acknowledges it.

A normal receipt has status `share_accepted`. A network-winning receipt has
status `block_accepted` and includes the block ID. Both include the durable
share sequence and share hash. Repeating an already accepted submission
returns the same receipt, which makes retry after a lost HTTP response safe.
The official client checks that the receipt height and share hash match its
submitted work, then requests a fresh verified template so the next search
commits to the latest public window.

Jobs are short-lived, memory-bounded, and invalid after their parent tip is no
longer current. A share whose verification overlaps a tip change may still be
counted if it passed the current-tip check before the change. Work submitted
after the coordinator observes the new tip is rejected as stale.

### Inspect the window

`GET /api/v2/pool/status` returns the current tip, fee, coinbase maturity,
window bounds, address weights, accepted share records, next sequence, and
state hash. Share records contain payout address, job and nonce identity,
share hash, share target, tip identity, height, and acceptance time. They do
not contain IP addresses or worker names.

Observers can reproduce the state hash and payout weights from this response.
Miners should retain accepted receipts if they want their own independent
history.

## Official client

PPLNS is the default mode:

```text
btc09 mine-pool -pool https://btc09.org -address YOUR_09C_ADDRESS -worker home-pc
```

Remote solo remains explicit:

```text
btc09 mine-pool -mode solo -pool https://btc09.org -address YOUR_09C_ADDRESS
```

The desktop wallet uses PPLNS and fills its own public receive address. Wallet
keys, recovery words, and wallet files never go to the pool.

## Operator setup

Enable v1 and v2 on the same loopback listener:

```text
btc09 node \
  -solo-api 127.0.0.1:9010 \
  -pplns-state /opt/btc09/data/pplns-window.json \
  -pplns-window 256 \
  -pplns-max-addresses 64
```

The state directory must not be group or world writable. The ledger holds an
exclusive non-blocking file lock, rejects links and malformed state, writes
mode 0600, and replaces state with a file and directory sync before replying.
Only one coordinator may own a state file.

Back up the ledger while the coordinator is stopped. Restoring it with a
different network or different window bounds fails closed.

## Trust boundary

V2 removes pool custody and lets the client verify each advertised payout
template. It does not make a centralized coordinator impossible to censor,
go offline, or omit work before publishing a new committed window. The public
status record and miner receipts make that behavior observable. Independent
coordinators can run the same open protocol.
