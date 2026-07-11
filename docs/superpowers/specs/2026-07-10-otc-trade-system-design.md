# Bitcoin 09 OTC Trade System Design

Date: 2026-07-10
Status: approved for implementation

## Goal

Build a production-minded, two-sided OTC market where people can buy or sell
09C for common fiat currencies, stablecoins, BTC, ETH, or another validated
asset code. The service escrows only the seller's 09C. The buyer pays the
seller directly outside the bot.

The first release must be understandable in English, safe against duplicate
payouts and accounting races, recoverable after process or network failure,
and visible through a privacy-safe website feed. It must pass a controlled,
small-value end-to-end trade before it is announced as generally available.

## Custody Boundary

The service will not receive or hold AUD, USD, CNY, USDT, USDC, BTC, ETH, bank
funds, or other settlement assets. It records the agreed asset, network, and
payment method, but the parties exchange payment details privately and settle
that leg directly.

This is a deliberate scope and risk boundary. AUSTRAC says businesses
providing digital-currency exchange or virtual-asset services must register,
and FinCEN guidance treats accepting and transmitting currency or substitute
value for others as potential money transmission. Holding both sides would
turn this project into a materially larger exchange and compliance system.
The relevant primary guidance is:

- https://www.austrac.gov.au/about-us/record-our-actions/virtual-asset-registration-actions
- https://www.fincen.gov/resources/statutes-regulations/guidance/application-fincens-regulations-persons-administering
- https://www.fincen.gov/system/files/2019-05/FinCEN%20CVC%20Guidance%20FINAL.pdf

The software will describe itself as experimental 09C-only escrow, not an
exchange, payment processor, guaranteed-safe service, or licensed custodian.
A legal review remains required before charging public fees or materially
scaling custody.

## Product Decisions

- Support both sell offers (WTS) and buy offers (WTB).
- Use a fixed 09C amount and a fixed total settlement amount per order.
- Offer common settlement assets through autocomplete: AUD, USD, EUR, GBP,
  CNY, JPY, USDT, USDC, BTC, ETH, SOL, LTC, DOGE, and BNB.
- Permit a custom uppercase asset code matching `[A-Z0-9._-]{2,12}`.
- Record an optional settlement network such as TRC20, ERC20, BEP20, Bitcoin,
  Solana, bank transfer, PayID, Wise, PayPal, Alipay, or WeChat Pay.
- Never store bank account numbers, wallet private keys, seed phrases, payment
  screenshots, or other off-chain payment credentials in the public feed.
- Keep the bot interface and all structured order records in English.
- Structured order actions render Chinese and other users' trade data in the
  same English format. Free-form chat translation is an optional adapter, not
  a dependency of custody or order execution.
- Use Discord application commands, buttons, selects, and modals for the main
  flow. Discord documents these as native interaction surfaces suitable for
  structured input: https://docs.discord.com/developers/platform/components
- Start the pilot at 0% service fee with configurable order and aggregate
  liability caps. Enable fees only after the accounting path, legal position,
  and operational process have been reviewed.

## User Flows

### WTS: seller makes an offer

1. Seller creates a sell offer with 09C amount, total price, settlement asset,
   network/payment method, and payment window.
2. Bot creates one fresh 09C deposit address and an `awaiting_deposit` order.
3. Seller deposits the quoted requirement: the buyer's net 09C amount plus
   one outbound network fee and any disclosed service fee. The bot waits for spendable balance
   at that address before publishing the order as `open`.
4. Buyer accepts. The order atomically changes from `open` to `matched`.
5. Parties exchange payment details privately. Buyer pays seller directly.
6. Buyer confirms payment sent. Seller confirms payment received.
7. Bot atomically reserves the release and sends escrowed 09C to the buyer's
   previously configured address.
8. Order becomes `broadcast`, then `completed` after chain reconciliation.

### WTB: buyer makes an offer

1. Buyer creates a buy offer with 09C amount, total price, settlement asset,
   network/payment method, and payment window. No outside funds are deposited.
2. Seller accepts. The order atomically assigns the seller, creates a fresh
   09C deposit address, and changes to `awaiting_deposit`.
3. Seller deposits the quoted requirement. Only after verification does the
   order become `matched` and tell the buyer to pay.
4. Confirmation, release, dispute, and completion follow the WTS flow.

### Cancellation and disputes

- An unaccepted WTB offer can be cancelled without a transfer. After a seller
  accepts, WTB never reopens: zero-credit cancellation closes it, while any
  partial/full seller credit is refunded before terminal closure when it exceeds
  the disclosed network fee. A residual at or below that fee remains a visible
  `recovery_hold` liability until topped up; it is never written off or paid from
  another customer's escrow. A new seller requires a new WTB and deposit address.
- A WTS order awaiting deposit can cancel without a transfer only at zero
  credit; partial confirmed credit becomes a recovery refund when positive after
  fee, otherwise it remains in `recovery_hold` until a top-up.
- An open, funded WTS cancellation returns the buyer-net amount plus any
  service-fee reserve; the separately quoted network-fee buffer pays the
  refund transaction.
- A buyer may leave a matched WTS trade before either payment flag; the same
  seller escrow can return to open. This rule never reopens an accepted WTB.
- Once either side claims payment movement, cancellation becomes a dispute.
- A matched trade times out into `disputed`; it never auto-pays or auto-refunds.
- Admin resolution requires an explicit buyer/seller outcome and a reason.
- Any ambiguous wallet result becomes `transfer_uncertain` and cannot be
  retried until an administrator reconciles the transaction and liabilities.

## State Model

Orders use explicit states:

```text
awaiting_deposit -> open -> matched -> release_reserved -> broadcast -> completed
         |           |        |              |
         |           |        +-> disputed <-+
         |           +-> refund_reserved -> refunded
         +-> cancelled
```

Additional states are `deposit_expired`, `recovery_hold`,
`transfer_failed_safe`, and `transfer_uncertain`. State changes that can lead to a wallet send use a
single conditional SQL update inside `BEGIN IMMEDIATE`. Only the caller that
successfully reserves the state may invoke the wallet.

Confirmation flags are stored separately from order state. The service must
reload the row inside the same transaction before deciding that both sides
confirmed.

## Persistence and Ledger

SQLite remains suitable for the current single-process service, but database
access moves behind a store module. WAL mode, foreign keys, busy timeout, and
transaction helpers are enabled on every connection.

Core tables:

- `users`: Discord ID, display name, validated 09C receive/refund address,
  timestamps.
- `orders`: side, maker, buyer, seller, 09C amount, total price, settlement
  asset, network/payment method, state, confirmation flags, deadlines, fresh
  deposit address, and timestamps.
- `deposit_scans`: durable evidence that one complete, validated address-history
  snapshot was applied at a specific canonical tip. Scan rows are append-only.
- `deposit_credits`: one immutable identity per canonical 09C output credited
  to an order deposit address. Identity is `(network, txid, vout)` and includes
  exact integer units, block anchor, confirmation/maturity state, first/last
  observation, and whether the output is still canonical. Identity, amount,
  credit time, and already-allocated bucket capacity can never be reduced or
  deleted; newer chain observations are accepted only from a newer matching
  scan record.
- `transfers`: one row per release, refund, dispute resolution, or fee
  withdrawal; includes a lifetime-unique operation key, immutable amount,
  network fee and destination, operation state, full transaction ID, command
  result classification, and timestamps.
- `transfer_credit_allocations`: exact main/recovery credit units discharged by
  a transfer only once that transfer is canonically confirmed.
- `withdrawals`: retained for migration compatibility and replaced by transfer
  rows for new fee withdrawals.
- `audit_events`: append-only state changes and admin actions without secrets.

Order quote economics and all credited/allocated accounting evidence are
append-only or narrowly monotonic at the database boundary. A direct SQL
writer cannot rewrite an old quote, outpoint, signed transfer, confirmation
anchor, or audit event to change liability after the fact. All machine fields
are canonical ASCII with byte-length and embedded-NUL checks, not only display
length checks.

Customer liability is derived from durable credits and confirmed allocations,
not from an order-state list and never by adding an order balance to its transfer
row. For each order it is `credited output units - credit units allocated to
canonically confirmed transfers`. Release allocations include the payout,
network fee, and earned service fee; refund/recovery allocations include only
the payout and network fee. Partial, excess, and late credits therefore remain
liabilities without special-case state filters. A post-credit
reorg freezes the service and retains the obligation for reconciliation instead
of silently reducing it. A customer send is refused unless that order covers its
immutable payout plus fee and the aggregate wallet remains solvent for every
other liability. The already-reserved outbound fee is not counted a second time.

The public order amount is the net 09C the buyer receives. The seller's quoted
deposit requirement is `net amount + outbound network fee + service fee`.
For a successful release, the buyer receives the net amount, the chain fee is
paid from its reserved buffer, and only the service-fee component is revenue.
For a refund, the chain fee is paid from the same buffer and the remaining
deposit is returned. Fee withdrawals reserve both the requested amount and
their own network fee atomically.

Underpayments remain `awaiting_deposit`. Overpayments never increase the order
amount; the excess becomes a separately recorded refund liability. A deposit
that arrives after cancellation or expiry becomes a recovery case and cannot
silently reopen or match the order. Deposit addresses continue to be watched
until any late or excess funds are reconciled.

A recovery balance at or below its outbound network fee enters
`recovery_hold`; it is not written off and no zero-value refund is attempted.
The address remains watched until a depositor top-up makes a positive refund
possible. The 0% pilot never takes the fee from another customer's escrow or
silently invents an operator subsidy.

## Wallet Boundary and Idempotency

Address-balance polling is not an accounting source. The wallet selects UTXOs
across all of its keys and confirmed spends remove outputs from the current
address balance, so polling can miss a deposit that was spent between checks.
The live node exposes a consistent current-best-chain snapshot at
`GET /api/v1/address/{address}/outputs`. It returns all canonical outputs to the
address, including spent outputs, with full `txid:vout`, integer units, block/tip
anchors, confirmations, coinbase maturity, and confirmed spend identity. The
bot treats a timeout, malformed response, incomplete snapshot, or non-200 as
unknown and never as a zero balance. Pilot deposits require a configurable
confirmation depth (default six) and coinbase maturity where applicable.

Before every wallet claim and preparation, the service takes one live tip and
fetches every watched deposit address against that exact expected tip. It applies
the complete all-address batch in one database transaction only if the watched
set is unchanged, then verifies every address's latest append-only scan has that
same tip. A new order/address, timeout, mixed tip, or tip change invalidates the
barrier and restarts reconciliation. This common-tip barrier is carried into
`prepare-send`; per-address snapshots from different tips are never combined for
solvency.

Every best-chain output is recorded before it reaches credit depth. Spendable
but not-yet-credited outpoints are provisional customer funds: their units are
subtracted from usable wallet solvency and their outpoints are excluded from
coin selection. They cannot mask a deficit or fund a payout/fee withdrawal.
A post-credit reorg, unknown custodian spend, or failure to reconcile the full
watched set at one tip blocks all claims.

A locked structured wallet snapshot at that same tip enumerates every
wallet-controlled address and spendable outpoint, including the internal primary
change address and unused preallocations, and returns exact integer units plus a
deterministic wallet/UTXO-set hash. Solvency uses this complete snapshot, never
an order-address sum or human CLI text. Preparation must return the same snapshot
hash; any intervening key/address or UTXO-set change is a safe retry before the
signed transaction is attached.

The current CLI uses binary floating-point amounts, prints only a shortened
transaction ID, and does not prove a peer accepted the broadcast. It can also
read a persisted chain snapshot that lags the running node. The bot replaces
float parsing with exact plain-decimal integer conversion and splits the wallet
boundary into live-tip verification, prepare, durable persist, exact broadcast,
and trusted-node reconciliation:

- full transaction ID;
- exact amount and network fee in integer base units;
- a live tip hash/height supplied to preparation and an exact persisted-snapshot
  match before signing;
- a second live-tip read before attaching the signed bytes, plus a canonical
  ancestor check before every broadcast/rebroadcast;
- at least one successful peer write reported as `submitted`, followed by
  trusted local-node observation before returning `broadcast`;
- distinct errors for `safe_to_retry` and `uncertain`;
- no P2P/mempool side effect during preparation;
- full signed transaction and txid committed to SQLite with FULL durability
  before the first submit/peer write;
- idempotent lookup/rebroadcast of only those stored bytes after a crash;
- background reconciliation against the local chain/explorer before marking
  a transfer completed.

The P2P accept path distinguishes a newly added transaction from one already
known and relays only the former, preventing exact rebroadcast from creating a
gossip loop. The CLI returns successful peer-write counts and machine-readable
JSON, but a socket write is only `submitted`, never proof of mempool acceptance.
The live explorer exposes canonical tip, block ancestry, and transaction status;
only that trusted local node may promote a submitted transfer to `broadcast`.
The Discord layer never parses human log prose or sees signed transaction bytes.

Escrow keys live in a dedicated `BTC09_WALLET_PATH` outside the node chain data
directory. Every wallet reader/writer takes the same interprocess lock. Address
creation re-reads under the lock, writes a mode-0600 temporary file in the same
directory, fsyncs it, atomically replaces the wallet, fsyncs the directory, and
returns the address only after durability succeeds. The bot service is the sole
writer; operators do not use the escrow wallet with the general-purpose CLI.

Every transfer has one lifetime-unique operation row. Business logic first
creates a `queued` transfer atomically with the order transition. A worker may
claim at most one global wallet operation by changing `queued` to `reserved`;
only that winning caller may invoke the wallet. A repeated Discord interaction,
process restart, two admins, or two simultaneous confirmations can therefore
create and send at most one operation. A queued row is safe to claim after a
restart because no wallet call was authorized. A `reserved` row has no signed
transaction and cannot have reached the network. Preparation changes it to
`prepared` only in the same durable commit that stores the exact txid/bytes;
prepared and broadcast recovery can therefore query or rebroadcast the same
transaction without duplicate payment. Reserved/prepared/broadcast occupy one
global send lane, while any uncertain row blocks all claims. A structured
pre-sign failure may requeue the same immutable row; it never creates a
replacement operation with a new amount, kind, destination, or transaction.
If uncertainty appears after a different operation was claimed, that operation
may not attach signed bytes or advance from prepared to broadcast until the
uncertainty is reconciled. Truthful confirmation and exact recovery of the
uncertain transaction remain allowed.

The second valid party confirmation and creation of the buyer release operation
occur in one `BEGIN IMMEDIATE` transaction. WTS and WTB acceptance also use one
conditional transaction; WTB preallocates a fresh address before attempting the
claim so a losing caller can only orphan an unused address, never expose a
shared deposit address.

## Code Structure

The existing 1,300-line bot file will become a thin composition root. New
modules have one responsibility each:

```text
bot/btc09_otc_bot.py       process startup and Discord wiring
bot/otc/domain.py          values, validation, state rules, fee math
bot/otc/store.py           schema, migrations, atomic reservations, audit log
bot/otc/wallet.py          btc09 CLI adapter and result classification
bot/otc/service.py         order and transfer orchestration
bot/otc/discord_ui.py      commands, buttons, modals, English messages
bot/otc/public_feed.py     privacy-safe JSON projection and atomic write
bot/otc/translation.py     optional English translation provider interface
```

The translation adapter is never called by transfer logic. If no reliable
provider is configured, the bot remains fully functional and the translation
command reports that translation is unavailable. No unofficial scraping API
will be placed on the custody path.

## Discord Interface

The primary interface uses a grouped `/trade` command:

- `/trade sell`
- `/trade buy`
- `/trade list`
- `/trade view <order>`
- `/trade accept <order>`
- `/trade deposit <order>`
- `/trade confirm-sent <order>`
- `/trade confirm-received <order>`
- `/trade cancel <order>`
- `/trade dispute <order> <reason>`
- `/trade address <09c-address>`
- `/trade mine`

Buttons mirror safe, state-appropriate actions. Destructive or fund-moving
actions require an ephemeral confirmation step. Admin commands are separate,
permission checked, reasoned, and recorded in `audit_events`.

Existing commands remain as compatibility wrappers for one release where the
mapping is unambiguous. Help text directs users to `/trade`.

## Public Website Feed

The website reads an atomically replaced JSON projection containing:

- order ID, side, public status, 09C amount, total price, price per 09C;
- settlement asset, network/payment-method label;
- created, updated, matched, and completed timestamps;
- aggregate open/matched/completed/disputed counts.

It excludes Discord IDs, usernames, wallet addresses, deposit addresses,
payment coordinates, dispute text, and private evidence. The feed includes a
schema version and service health timestamp. The market page distinguishes
GitHub noticeboard offers from bot-escrow orders.

## Security and Operations

- Rotate credentials exposed in the prior session before public pilot use.
- Keep Discord secrets in `/etc/btc09/discord.env` mode 600.
- Run the bot as a dedicated unprivileged user with systemd hardening and only
  the specific wallet/data/feed paths writable.
- Back up the dedicated root-only hot-wallet key file and SQLite database before
  every deployment; keep the funded pilot caps low because this wallet is online.
- Never log tokens, private keys, payment details, or full off-chain evidence.
- Add structured journald errors for deposit, send, refund, release,
  withdrawal, translation, and reconciliation failures.
- Expose a local-only health check with database, explorer, wallet, feed age,
  pending liability, and uncertain-transfer status.
- Block new orders if reconciliation is unhealthy or liabilities do not match
  wallet spendable balance.

## Testing

Automated tests must cover:

- amount, asset, network, payment-method, address, and state validation;
- WTS and WTB happy paths;
- simultaneous accepts and confirmations;
- duplicate admin resolution, refund, and fee withdrawal attempts;
- process restart during reserved and broadcast transfers;
- stale persisted-chain snapshots, preparation-time reorgs, ambiguous database
  commits, duplicate P2P delivery, and systemd restarts with wallet children;
- concurrent wallet address creation and process death at each durable-write
  boundary;
- safe failure versus uncertain send classification;
- exact fee/liability math including network fees;
- deposit attribution and over/under-deposit handling;
- timeout-to-dispute behavior;
- public-feed privacy and atomic writes;
- migration from the existing empty v0.2.1 database;
- Discord command authorization and idempotency.

The live gate uses a separate pilot wallet and the smallest practical 09C
amount. It exercises sell, deposit, accept, both confirmations, payout,
refund, dispute resolution, restart recovery, feed readback, and service logs.
No public announcement occurs until balances and transaction IDs reconcile.

## Rollout

1. Fix and release the active P2P connection-storm defect.
2. Commit this design and a task-level implementation plan.
3. Build pure domain/store/wallet modules with failing tests first.
4. Migrate the existing empty production database from a backup.
5. Deploy with order creation disabled and verify commands/health.
6. Run controlled funded end-to-end cases on a pilot wallet.
7. Rotate exposed credentials and enable a capped 0% fee pilot.
8. Update website and Discord guidance after readback proves the pilot works.
9. Obtain legal review before enabling fees or increasing custody limits.

## Non-Goals for This Release

- Custody of fiat, stablecoins, BTC, ETH, or any settlement asset other than
  09C.
- Automated bank/card/PayPal/Alipay/WeChat or external-chain verification.
- An order-matching engine, market orders, pooled liquidity, or official price.
- KYC identity verification or a claim that the service satisfies exchange,
  money-transmission, or virtual-asset licensing obligations.
- Automatic translation that relies on an untrusted public scraping service.
