# Bitcoin 09 exchange integration

This is the operator contract for Bitcoin 09 (`09C`) deposits and withdrawals.
09C is a native UTXO chain, not a token. Integration uses the reference
`btc09` binary and its versioned, read-only chain API plus strict JSON wallet
commands.

The current supported release is [v0.1.23](https://github.com/krutftw/bitcoin09/releases/tag/v0.1.23).
Open a GitHub issue for integration questions that do not contain account data,
wallet files, signed transactions, or credentials. Private exchange contact can
continue through the exchange's listing ticket.

## Support boundary

Supported integration surfaces:

- versioned `GET /api/v1/*` chain queries;
- JSON `wallet new` and `wallet snapshot` commands;
- JSON `prepare-send`, `inspect-tx`, and `broadcast-tx` commands;
- checksummed Linux amd64 and arm64 release binaries.

The HTML explorer and the legacy `/api/status` endpoint are public convenience
interfaces. Do not build deposit accounting against their presentation fields.
There is no public wallet RPC and there should not be one. Wallet commands run
locally under the exchange service account.

## Release verification

Download the Linux binary and checksum file from the same release:

```bash
VERSION=v0.1.23
curl -fLO "https://github.com/krutftw/bitcoin09/releases/download/$VERSION/btc09-linux-amd64"
curl -fLO "https://github.com/krutftw/bitcoin09/releases/download/$VERSION/SHA256SUMS.txt"
sha256sum --check --ignore-missing SHA256SUMS.txt
install -m 0755 btc09-linux-amd64 /usr/local/bin/btc09
btc09 version
```

The expected version output includes `reference node v0.1.23`. Pin an exact
release in production. Do not run an unreviewed binary directly from `latest`.

## Node and wallet layout

Use a dedicated unprivileged service account and separate the wallet from the
public website and exchange application users:

```bash
install -d -o btc09 -g btc09 -m 0700 /var/lib/btc09-exchange
install -d -o btc09 -g btc09 -m 0700 /var/lib/btc09-exchange/node
```

Example paths:

```bash
DATADIR=/var/lib/btc09-exchange/node
WALLET=/var/lib/btc09-exchange/hot-wallet.json
```

Create the wallet once with the human network name. This creates the first
address when the file does not exist:

```bash
sudo -u btc09 btc09 wallet new \
  -network mainnet \
  -datadir "$DATADIR" \
  -wallet-file "$WALLET"
chmod 0600 "$WALLET"
```

Run one non-mining node. Keep P2P reachable if possible, but bind the explorer
API to localhost:

```bash
sudo -u btc09 btc09 node \
  -network mainnet \
  -datadir "$DATADIR" \
  -wallet-file "$WALLET" \
  -listen 0.0.0.0:9009 \
  -explorer 127.0.0.1:8009 \
  -seeds seed.btc09.org:9009 \
  -no-update-check
```

Do not add `-mine` to an exchange wallet node. Restrict access to port 8009 at
the operating-system and cloud-firewall layers even though the API is
read-only.

## Versioned chain API

All v1 responses use `Content-Type: application/json`, reject unknown query
parameters, and return an explicit `schema_version` and `network`. Mainnet is
identified as `btc09-mainnet`.

Current tip:

```bash
curl -fsS http://127.0.0.1:8009/api/v1/tip
```

```json
{"schema_version":1,"network":"btc09-mainnet","tip":{"hash":"64-lowercase-hex","height":7374}}
```

Canonical block membership:

```text
GET /api/v1/block/{64-lowercase-hex-block-hash}
```

The response contains `found`, a `block` object with `hash`, `height`, and
`canonical`, plus the exact tip used for the lookup.

Transaction state:

```text
GET /api/v1/transaction/{64-lowercase-hex-txid}
```

`status` is `unknown`, `mempool`, or `confirmed`. A confirmed transaction has a
canonical block anchor and `confirmations`. Unknown and mempool transactions
have zero confirmations and no block anchor.

Address outputs at an exact tip:

```text
GET /api/v1/address/{base58-address}/outputs?expected_tip_hash={hash}&expected_tip_height={height}
```

A successful response has `complete: true`, repeats the requested tip, and
returns canonical outputs ordered by block height, transaction index, and
output index. Each output includes:

- `txid`, `transaction_index`, and `vout`;
- integer `amount_units` where 100,000,000 units equal 1 09C;
- canonical block hash and height;
- `confirmations`, `coinbase`, and `mature`;
- `spent_by`, which is `null` or a confirmed spending transaction anchor.

If the chain tip changed, the endpoint returns HTTP 409 with `complete: false`
and the new tip. Discard the entire scan response and retry from a new tip.

## Deposit address allocation

Give every customer or deposit intent its own address. After the wallet file
exists, use the machine network name and JSON mode:

```bash
btc09 wallet new \
  -wallet-file "$WALLET" \
  -network btc09-mainnet \
  -json
```

```json
{"ok":true,"schema_version":1,"network":"btc09-mainnet","stage":"wallet_new","address":"4k..."}
```

Store the returned address before showing it to a customer. Back up the updated
wallet after allocating new addresses. Never reuse an address for a different
customer, asset, or network.

Machine-command failures return a bounded JSON object with `ok: false`, a
stable `stage`, and a safe `error_code`. Detailed errors go to neither stdout
nor the public API.

## Tip-pinned deposit scanning

For each scan batch:

1. Read `/api/v1/tip` from the exchange's local node.
2. Query every watched address with that exact hash and height.
3. Abort the whole batch on any HTTP 409, timeout, malformed response, network
   mismatch, or changed tip.
4. Deduplicate credits by `(network, txid, vout)`.
5. Store the block hash and height with every observation.
6. Recheck unfinalized deposits on every new tip.

Credit an output only when:

- the response is complete and matches the requested mainnet tip;
- `amount_units` is positive and within the 21M supply bound;
- `spent_by` is null at the observation tip;
- `confirmations` meets the exchange policy;
- a coinbase output also has `mature: true`.

Start with **100 confirmations** for deposits while the network is young. The
exchange owns the final policy and should raise it if observed hashrate,
distribution, or reorg risk warrants it. Coinbase rewards cannot be spent for
100 blocks under consensus rules regardless of the exchange policy.

## Confirmations and reorg handling

Treat confirmations as a property of an output at a specific tip, not as a
monotonic counter. Before final credit, verify that the stored block remains
canonical and the output is still present in a complete tip-pinned address
snapshot.

On a tip mismatch or reorg:

1. stop credit finalization and withdrawal selection;
2. fetch the new tip;
3. rescan from the last internally finalized height;
4. reverse only credits that have not crossed the exchange's finality policy;
5. require manual review if a previously finalized deposit disappears.

The chain selects the branch with the most cumulative proof of work, not simply
the greatest height.

## Withdrawal preparation and broadcast

Fetch the local tip, then create a wallet snapshot bound to it:

```bash
btc09 wallet snapshot \
  -wallet-file "$WALLET" \
  -datadir "$DATADIR" \
  -network btc09-mainnet \
  -expected-tip-hash "$TIP_HASH" \
  -expected-tip-height "$TIP_HEIGHT" \
  -json
```

The result includes addresses, canonical wallet outpoints, total spendable
units, and `wallet_snapshot_hash`. Keep an exchange-side set of reserved
outpoints for every prepared but unresolved withdrawal.

Prepare a signed transaction while excluding those reservations:

```bash
printf '%s' "$EXCLUDED_OUTPOINTS_JSON" | btc09 prepare-send \
  -to "$DESTINATION" \
  -amount "$AMOUNT" \
  -fee "$FEE" \
  -datadir "$DATADIR" \
  -network btc09-mainnet \
  -wallet-file "$WALLET" \
  -expected-tip-hash "$TIP_HASH" \
  -expected-tip-height "$TIP_HEIGHT" \
  -exclude-outpoints-json - \
  -json
```

`EXCLUDED_OUTPOINTS_JSON` is a JSON string array of lowercase `txid:vout`
values. The response includes the exact destination, amount, fee, transaction
ID, selected outpoints, snapshot tip, snapshot hash, and signed transaction
hex.

Before broadcast, independently inspect the exact bytes and compare every
input and output with the approved withdrawal:

```bash
printf '%s' "$SIGNED_TX_HEX" | btc09 inspect-tx \
  -tx-hex - \
  -network btc09-mainnet \
  -json
```

Atomically reserve the selected outpoints in the exchange database, then
broadcast:

```bash
printf '%s' "$SIGNED_TX_HEX" | btc09 broadcast-tx \
  -tx-hex - \
  -expected-txid "$TXID" \
  -datadir "$DATADIR" \
  -network btc09-mainnet \
  -seeds seed.btc09.org:9009 \
  -json \
  -require-broadcast=true
```

Keep the reservation until the transaction is confirmed, explicitly rejected,
or replaced by an operator-approved recovery transaction. Poll the versioned
transaction endpoint; do not infer success from the CLI process exiting alone.

## Backups and hot-wallet controls

- Keep the wallet file mode at `0600` under the dedicated service account.
- Encrypt wallet backups outside the server and test restoration on an offline
  host.
- Back up after address allocation and before software or host migrations.
- Keep only the operational withdrawal float online. Store excess 09C in a
  separately controlled cold wallet.
- Put withdrawal rate, amount, and destination controls in the exchange layer.
- Require independent review for hot-wallet limit changes and recovery sends.
- Never put the wallet file, signed transaction hex, API credentials, or
  private support-ticket contents in logs or issue trackers.

## Read-only integration smoke test

From a repository checkout:

```bash
python3 tools/exchange/btc09_exchange_smoke.py \
  --base-url http://127.0.0.1:8009
```

Optionally validate one allocated address with a tip-pinned scan:

```bash
python3 tools/exchange/btc09_exchange_smoke.py \
  --base-url http://127.0.0.1:8009 \
  --address "$DEPOSIT_ADDRESS"
```

The tool is read-only, has no third-party Python dependencies, and never reads
the wallet file. A successful result is one compact JSON line with `ok: true`,
the network, height, and tip hash.

Direct production read-back is also available:

```bash
curl -fsS https://explorer.btc09.org/api/v1/tip
```

## Incident recovery

After a node outage or chain-store restore:

1. keep deposits and withdrawals paused;
2. sync from at least two reachable peers;
3. compare the local tip with the public explorer and another independent node;
4. run the read-only smoke test;
5. rescan watched addresses from the last finalized height;
6. reconcile reserved withdrawal outpoints and transaction states;
7. resume deposits first, then withdrawals after operator review.

Do not restore an old wallet file over a newer one. If backup histories conflict,
keep both copies offline and reconcile their address sets before resuming.
