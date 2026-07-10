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

- An unaccepted WTB offer can be cancelled without a transfer.
- A WTS order awaiting deposit can be cancelled without a transfer.
- An open, funded WTS cancellation returns the buyer-net amount plus any
  service-fee reserve; the separately quoted network-fee buffer pays the
  refund transaction.
- A buyer may leave a matched trade before claiming payment was sent; the
  order can return to open only if the seller has not marked payment received.
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

Additional failure states are `deposit_expired`, `transfer_failed_safe`, and
`transfer_uncertain`. State changes that can lead to a wallet send use a
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
- `transfers`: one row per release, refund, dispute resolution, or fee
  withdrawal; includes amount, network fee, destination, operation state,
  full transaction ID, command result classification, and timestamps.
- `withdrawals`: retained for migration compatibility and replaced by transfer
  rows for new fee withdrawals.
- `audit_events`: append-only state changes and admin actions without secrets.

The liability calculation includes every funded order not conclusively paid
or refunded. A send is refused unless wallet spendable balance covers all
liabilities plus the new network fee. The network fee is never silently taken
from another active order.

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

## Wallet Boundary and Idempotency

The current CLI prints only a shortened transaction ID and does not prove a
peer accepted the broadcast. The bot needs a structured wallet adapter:

- full transaction ID;
- exact amount and network fee in integer base units;
- at least one successful peer write before returning `broadcast`;
- distinct errors for `safe_to_retry` and `uncertain`;
- no automatic retry after a transaction may have been signed or broadcast;
- background reconciliation against the local chain/explorer before marking
  a transfer completed.

The P2P node and CLI may be extended to return a successful broadcast count
and machine-readable JSON. The Discord layer never parses human log prose.

Every transfer has a unique operation row. A repeated Discord interaction,
process restart, two admins, or two simultaneous confirmations can reserve at
most one transfer. Restart recovery scans reserved/broadcast/uncertain rows
and reconciles them before allowing new wallet operations.

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
- Back up the encrypted wallet and SQLite database before every deployment.
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
