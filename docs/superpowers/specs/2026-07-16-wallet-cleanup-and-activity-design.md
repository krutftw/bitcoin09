# BTC09 Wallet Cleanup and Activity Design

**Status:** Approved 2026-07-16  
**Target:** v0.1.32  
**Surfaces:** desktop wallet, light-wallet gateway, Discord, website, GitHub release

## Goal

Make the BTC09 wallet feel like a normal money app for miners and first-time
users. A person should be able to understand what is ready to spend, see what
happened to a payment, send the available balance without doing fee maths, and
combine many small mining payments without learning UTXO terminology.

The normal interface stays short and plain. Transaction internals remain
available only as optional detail.

## Evidence and current gaps

The live BTC09 Discord has produced the same questions repeatedly:

- how to create a wallet without a terminal;
- whether the official miner works and where to download it;
- why a mined balance cannot be spent immediately;
- how to find a sent transaction; and
- how to consolidate many small payments.

The explorer already supports TXID search and full confirmed address history as
of v0.1.30. Rebuilding those pages would duplicate finished work. The desktop
wallet still lacks an activity view, persistent payment status, a Max action,
and a purpose-built cleanup flow. Its `TOTAL BALANCE` label is also inaccurate:
the displayed figure is the mature balance that is ready to spend.

External research supports keeping the simple and advanced layers separate:

- Trezor describes manual coin control as advanced, defaults to automatic coin
  selection, and warns that combining unrelated inputs can reveal ownership
  links: https://trezor.io/learn/supported-assets/bitcoin/coin-control-in-trezor-suite-choose-which-utx-os-to-spend
- BlueWallet groups normal wallet use around backup, send/receive, transaction
  status, pending transactions, history, and optional coin control:
  https://bluewallet.io/docs/
- Sparrow exposes detailed history and transaction construction for advanced
  users, but keeps that detail organized around understanding a send:
  https://www.sparrowwallet.com/features/
- Reddit questions repeatedly show that using a normal send form for
  consolidation makes `Full balance`, the destination, and the selected inputs
  unclear:
  https://www.reddit.com/r/Bitcoin/comments/1alybf8/
- BlueWallet's issue tracker contains recurring reports around missing history,
  full-balance behavior, and maximum-send behavior, including issues #7509,
  #7017, #4985, and #8265.

Reddit, GitHub, and social posts are discovery inputs, not protocol authority.
Security and transaction behavior must still be grounded in the BTC09 source
and primary wallet documentation.

## Product decisions

### 1. Automatic wallet cleanup

The user-facing name is **Combine small payments**. `UTXO`, `outpoint`, `coin
control`, and raw transaction size do not appear in the normal flow.

The action is available when at least two mature outputs received by the same
wallet address can be combined for more than the network fee. It becomes a
visible recommendation at 20 eligible outputs or after a normal send fails
because too many inputs would be required. Below that threshold it remains
available under Wallet tools without prompting the user.

One cleanup pass:

1. groups spendable outputs by the wallet address that owns them;
2. chooses the group with the most eligible outputs, with deterministic address
   ordering for ties;
3. selects the smallest outputs first;
4. includes as many as safely fit under the existing 10,000-byte signed
   transaction limit;
5. sends their combined value, minus the fee, back to that same address in one
   output; and
6. creates no separate change output.

The builder must measure the final encoded, signed transaction. The interface
must not rely on a hard-coded input count because encoding boundaries and future
transaction changes can alter the safe maximum.

The wallet never combines outputs from different receive addresses by default.
That prevents the cleanup tool from creating a new cross-address ownership link.
V2 wallets currently use one stable receive address, while legacy V1 wallets
may contain several addresses; both formats use the same rule.

Only mature, unspent outputs are eligible. Unknown remote outputs, immature
mining rewards, excluded outputs, duplicate outpoints, and amounts that do not
cover the fee are rejected before a preview is created.

If more work remains, the result says **More cleanup will be available after
this confirms.** A second pass cannot be built from a stale pre-confirmation
snapshot.

### 2. Cleanup review and confirmation

Cleanup uses the existing local-signing and explicit-confirmation boundary. It
does not broadcast automatically.

The preview contains:

- payments combined;
- amount returned to this wallet;
- network fee;
- receiving address, shortened by default with a Copy action;
- current chain height;
- the existing six-character check code; and
- whether another pass will be useful after confirmation.

Primary copy:

- Heading: **Combine small payments**
- Explanation: **Turn many small mining payments into one larger wallet entry.
  Your 09C stays in this wallet.**
- Confirmation button: **Combine payments**
- Empty state: **No cleanup needed.**
- Privacy disclosure: **BTC09 only combines payments already received at the
  same address.**

Individual outpoints and signed bytes are not shown. An optional `How this
works` disclosure may explain that the wallet sends funds back to itself.

Pending previews gain an internal purpose of `send` or `cleanup`. The normal
send confirmation endpoint must reject a cleanup preview, and the cleanup
confirmation endpoint must reject a normal send preview. Both retain the
existing expiry, in-flight, CSRF, and exact-transaction protections.

### 3. Clear balance summary

The main balance label changes from `TOTAL BALANCE` to **READY TO SEND**.

When the wallet has immature mining rewards, a secondary line reads:

**Mining rewards waiting: X 09C**

An optional explanation says that mined rewards become ready after 100 blocks.
The UI does not call those funds missing, frozen, pending approval, or locked.

The gateway snapshot adds validated additive fields for immature units and the
count of mature spendable outputs. Existing response fields and schema version
remain compatible. Full mode derives the same values from the local canonical
chain. Overflow, maturity, ownership, tip identity, ordering, and response-size
checks remain mandatory.

### 4. Max send

The Amount field gains a small **Max** action. It asks the backend for a normal
send preview using the greatest currently spendable amount after the entered
fee; the browser does not calculate this from a displayed decimal balance.

The final review shows the exact amount and fee as usual. If the full balance
requires too many inputs, the user sees:

**This wallet has many small payments. Combine some first, then try Max again.**

That message links directly to Wallet tools.

### 5. Wallet activity

Add an **Activity** tab without turning the wallet into a block explorer. It
shows the most recent 50 wallet transactions, newest first.

Rows use plain types:

- Received
- Sent
- Mining reward
- Wallet cleanup

States use:

- Waiting for confirmation
- Confirmed
- Mining reward waiting
- Ready to send

Each row shows the net 09C amount, state, block or confirmation detail, a
shortened TXID, Copy, and **Open in explorer**. Expanded detail may show the
full TXID, fee, and address. It does not show raw scripts or input lists.

Fast mode requests normalized activity for the wallet's public addresses from
the official wallet gateway. This does not disclose private keys or recovery
words, but it has the same address-privacy tradeoff already disclosed for Fast
mode. Full mode derives activity locally and must not call the public activity
service.

The activity result aggregates every wallet address before calculating a net
amount, so internal change is not shown as a second incoming payment. A
transaction whose spendable inputs and outputs all belong to the wallet is
classified as Wallet cleanup. Confirmed and mempool activity are included so a
submitted payment remains visible after an app restart.

The public activity response is bounded by address count, transaction count,
request size, response size, canonical tip, network, and deterministic order.
The desktop client validates every field before displaying it.

### 6. Navigation and visual hierarchy

Keep the existing BTC09 visual language. Do not add gradients, oversized hero
copy, dense dashboards, or a permanent technical panel.

The action order is:

1. Receive
2. Send
3. Activity
4. Mine
5. Backup

Desktop may show all five in one row. Narrow layouts use a clean wrapped grid,
with labels remaining at normal text size. Wallet cleanup lives inside Activity
under a compact Wallet tools section and appears near the top only when it is
recommended.

The final implementation must be visually inspected at desktop and narrow
mobile-sized viewports. No heading may dominate the balance, and secondary
explanations must remain shorter and quieter than the action they support.

### 7. Discord discovery

Add two guild slash commands:

- `/wallet`: official release link, one sentence for Windows/macOS/Linux, and a
  reminder to write down recovery words and never send them to anyone.
- `/mine`: directs users to the Mine tab in the official wallet, explains that
  the included CPU miner is open source, and links to the mining guide.

Replies are ephemeral and concise. The command definitions remain present
through both Discord bot sync paths so a service restart cannot remove them.

Update the existing `start-here` and `resources` seeded messages in place. Do
not create duplicate messages or make a separate pre-release announcement.
After the release and live checks pass, publish one short release announcement.

## API shape

Desktop-local routes:

- `GET /api/v1/activity`
- `POST /api/v1/send/max-preview`
- `POST /api/v1/maintenance/cleanup/preview`
- `POST /api/v1/maintenance/cleanup/confirm`

The remote wallet gateway gains one bounded activity endpoint. Exact request
and response structs will be fixed in the implementation plan. All amounts use
integer base units on service boundaries; decimal parsing remains confined to
user-input adapters.

Cleanup and Max use the same wallet snapshot validation in Fast mode and the
same canonical-tip snapshot in Full mode as normal send. The transaction is
signed only on the user's computer. The gateway receives a signed transaction
only at the existing broadcast boundary.

## Error language

Errors tell the user what to do next and omit internals:

- **No cleanup needed.**
- **The available payments are too small to cover the fee.**
- **The wallet changed while this was being prepared. Try again.**
- **This cleanup preview expired. Review it again.**
- **The wallet service is temporarily unavailable. Your funds are safe; try
  again.**
- **No peer accepted the transaction. Check the connection and try again.**

Logs and optional technical detail retain the underlying error for diagnosis.

## Security and privacy requirements

- Recovery words, private keys, wallet passwords, wallet file contents, and
  signed transaction bytes never enter activity responses, support reports,
  browser storage, Discord replies, or logs.
- Cleanup signs locally and requires a fresh preview plus explicit confirmation.
- Remote snapshots and activity are validated for network, tip, ownership,
  uniqueness, canonical formatting, money range, maturity, deterministic order,
  and size bounds.
- Cross-address consolidation is forbidden by the default builder.
- A cleanup preview cannot be confirmed through the normal send route or vice
  versa.
- Concurrent confirmation remains single-flight and idempotent for the exact
  prepared transaction.
- Full mode remains private and never falls back silently to the public activity
  service.
- V1 wallet files remain byte-for-byte compatible. V2 encryption and recovery
  derivation do not change.

## Testing and verification

Implementation is test-driven and must cover:

- deterministic same-address grouping and smallest-first selection;
- no cross-address input mixing;
- exact signed-size batching at and around 10,000 bytes;
- insufficient fee value, one-output, immature, duplicate, foreign, excluded,
  overflow, and stale-snapshot rejection;
- typed preview separation, expiry, concurrency, and broadcast failure;
- Max amount calculation by the backend;
- multi-address activity aggregation, change handling, cleanup classification,
  mining maturity, mempool state, and stable ordering;
- malformed or oversized remote activity responses;
- locked, missing, V1, and V2 wallet behavior;
- desktop API, CSRF, and public-error contracts;
- frontend copy, tab behavior, keyboard use, narrow layout, and no raw secrets;
- `/wallet` and `/mine` command registration and restart-safe guild sync; and
- existing wallet, consensus, explorer, gateway, miner, OTC, Discord, site, and
  deployment suites.

Before release:

1. run the fresh full Go, Python, Node, website, and deployment suites;
2. build every supported release artifact and inspect archive contents;
3. visually inspect the app at desktop and narrow viewport sizes;
4. test a real Fast-mode cleanup on regtest or an isolated funded test wallet;
5. deploy and read back the live wallet gateway and Discord commands;
6. publish signed/checksummed GitHub artifacts and read back the release;
7. update the website download and release copy; and
8. make one Discord announcement only after the live checks pass.

## Non-goals for v0.1.32

- manual per-output coin control;
- consolidation across different receive addresses;
- automatic or scheduled consolidation;
- multi-recipient sends;
- fiat price claims before a reliable public market exists;
- hardware-wallet support;
- transaction labels or CSV export;
- a browser wallet holding private keys; and
- Android or iOS key custody.

Native mobile remains a separate phase because OS key storage, app signing,
recovery testing, and store distribution need their own design and release
gate. Later research candidates include transaction labels, CSV export, payment
links, QR scanning for sends, an address book, and a properly signed native
mobile wallet. They should be prioritized from verified BTC09 usage rather than
added as a generic wallet checklist.
