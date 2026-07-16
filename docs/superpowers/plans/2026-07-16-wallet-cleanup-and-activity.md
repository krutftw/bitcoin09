# Wallet Cleanup and Activity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship BTC09 v0.1.32 with safe same-address payment consolidation, backend-calculated Max send, understandable spendable and immature balances, a useful Activity view, and concise wallet/mining guidance in Discord.

**Architecture:** Preserve the existing strict `/api/wallet/v1/snapshot` contract and add a separate atomic wallet-view endpoint for richer state. Build consolidation and maximum-send transactions in the wallet package, expose typed preview/confirm operations through the app and desktop layers, and keep every broadcast behind an explicit confirmation. Reuse the explorer for transaction detail, use existing Discord services and idempotent seed-post updates, and deploy only after old-client compatibility, archive inspection, visual inspection, and live read-back pass.

**Tech Stack:** Go node, wallet, gateway, and desktop server; embedded HTML/CSS/JavaScript desktop UI; Node.js Discord stats bot; Python OTC bot and contract tests; GitHub Actions and GitHub Releases; systemd services on the existing DigitalOcean VPS.

## Global Constraints

- Keep `lightwallet.SnapshotResponse` and `/api/wallet/v1/snapshot` byte-shape compatible with v0.1.31 because strict clients reject unknown JSON fields.
- Never combine outputs controlled by different wallet addresses in one cleanup transaction.
- Never broadcast a send or cleanup from a preview request. Every broadcast needs a fresh, typed confirmation token.
- Measure the final signed transaction and enforce `core.MaxSignedTxBytes`; do not use a guessed fixed input count.
- Exclude confirmed outputs already spent by a mempool transaction from the ready-to-send balance.
- Treat unconfirmed change as activity, not immediately spendable state, because chained unconfirmed spending is not part of the wallet protocol.
- Keep copy short and ordinary. Do not expose raw scripts, signed hex, or outpoints in the UI.
- Update Discord seed posts in place through their existing markers. Do not create duplicate guidance or duplicate release announcements.
- Keep unrelated user changes intact and use focused commits after each green task.
- Do not publish, deploy, or announce v0.1.32 until the complete local and live verification gates pass.

---

### Task 1: Add one atomic canonical wallet view in the core

**Files:**
- Create: `core/wallet_view.go`
- Create: `core/wallet_view_test.go`
- Modify: `core/chain.go`
- Reference: `core/core_test.go`

- [ ] Add failing tests for spendable outputs, immature mining rewards, aggregate transaction activity, and mempool spends.

  Cover all of these cases in `core/wallet_view_test.go`:

  - confirmed ordinary receive is `received` with a positive net amount;
  - coinbase is `mining_reward` and remains immature until 100 confirmations;
  - owned inputs with an external output are `sent` with the wallet's negative net amount;
  - owned inputs with only owned outputs are `cleanup`;
  - a mempool item is returned before confirmed items with status `mempool`;
  - a confirmed output consumed in the mempool is absent from `SpendableOutputs`;
  - confirmed items are newest-first by height and transaction index;
  - activity is capped by the requested limit;
  - limits below zero or above 50 are rejected;
  - duplicate public-key hashes are harmless;
  - an empty address set returns a complete empty view at the current tip.

- [ ] Run the focused tests and confirm the new API is missing.

  Run: `go test ./core -run WalletView -count=1`

  Expected: FAIL with undefined wallet-view types or methods.

- [ ] Implement the public wallet-view model in `core/wallet_view.go`.

  Use these public contracts:

  ```go
  const MaxWalletActivityLimit = 50

  const (
      WalletActivityReceived     = "received"
      WalletActivitySent         = "sent"
      WalletActivityMiningReward = "mining_reward"
      WalletActivityCleanup      = "cleanup"
      WalletActivityMempool      = "mempool"
      WalletActivityConfirmed    = "confirmed"
  )

  type ImmatureOutputSnapshot struct {
      OutPoint      OutPoint
      AmountUnits   int64
      OwnerPKH      [20]byte
      OwnerIndex    uint32
      BlockHeight   int64
      Confirmations int64
  }

  type WalletActivityItem struct {
      TxID              Hash32
      Kind              string
      Status            string
      NetUnits          int64
      BlockHash         Hash32
      BlockHeight       int64
      Confirmations     int64
      BlocksUntilMature int64
      TransactionIndex  uint32
  }

  type WalletViewSnapshot struct {
      Network          string
      Complete         bool
      Tip              ChainTipSnapshot
      SpendableOutputs []SpendableOutputSnapshot
      SpendableUnits   int64
      ImmatureOutputs  []ImmatureOutputSnapshot
      ImmatureUnits    int64
      Activity         []WalletActivityItem
  }

  func (c *Chain) WalletViewForPKHs(pkhs [][20]byte, activityLimit int) (WalletViewSnapshot, error)
  ```

- [ ] Scan the canonical chain and mempool while holding one chain read lock.

  Requirements:

  - validate owner indexes, duplicate outpoints, and all amount additions with the existing money-range helpers;
  - build an ownership map for canonical outputs so input and output values can be aggregated per transaction;
  - collect mature unspent outputs and immature owned coinbase outputs separately;
  - calculate `Confirmations` as `tipHeight - blockHeight + 1`;
  - calculate `BlocksUntilMature` only for immature mining rewards;
  - classify activity using wallet-owned input and output totals;
  - mark confirmed outpoints referenced by valid mempool transactions as unavailable;
  - sort spendable outpoints by transaction hash and output index using the established snapshot ordering;
  - sort mempool activity first, then confirmed activity by descending block height and descending transaction index.

- [ ] Refactor `SpendableOutputsForPKHs` to share the locked scanner without changing its public signature.

  It must now exclude mempool-spent outputs while retaining the same network, tip, completeness, owner, and sorting guarantees.

- [ ] Run focused and package tests.

  Run:

  ```powershell
  go test ./core -run 'WalletView|SpendableOutputs' -count=1
  go test ./core -count=1
  ```

  Expected: PASS.

- [ ] Commit the core view.

  ```powershell
  git add core/wallet_view.go core/wallet_view_test.go core/chain.go
  git commit -m "Add atomic wallet balance and activity view"
  ```

### Task 2: Add the backward-compatible lightwallet view endpoint

**Files:**
- Modify: `lightwallet/types.go`
- Modify: `lightwallet/gateway.go`
- Modify: `lightwallet/client.go`
- Create: `lightwallet/view_test.go`
- Modify: `lightwallet/gateway_test.go`
- Modify: `lightwallet/client_test.go`

- [ ] Add a strict legacy compatibility test before changing gateway behavior.

  Decode a real snapshot response into the current `SnapshotResponse` with `json.Decoder.DisallowUnknownFields()` and assert the JSON keys remain exactly the existing snapshot keys.

- [ ] Add failing tests for `POST /api/wallet/v1/view`.

  Cover:

  - POST and `application/json` are required;
  - request body and address-count limits match the snapshot endpoint;
  - canonical address normalization and duplicate removal;
  - activity limits 0 through 50 are accepted and other values are rejected;
  - spendable and immature sums, counts, ownership, order, height, and confirmations are validated;
  - activity kind, status, amount range, order, limit, maturity, and transaction ID are validated;
  - trailing JSON and unknown fields are rejected;
  - legacy snapshot JSON remains unchanged.

- [ ] Run the focused tests and confirm the view contract is missing.

  Run: `go test ./lightwallet -run 'View|LegacySnapshot' -count=1`

  Expected: FAIL with missing view types, route, and client method.

- [ ] Add the view wire types in `lightwallet/types.go` without editing `SnapshotRequest` or `SnapshotResponse`.

  ```go
  const ViewPath = "/api/wallet/v1/view"

  type ViewRequest struct {
      Addresses     []string `json:"addresses"`
      ActivityLimit int      `json:"activity_limit"`
  }

  type ViewImmatureOutput struct {
      TxID          string `json:"txid"`
      Vout          uint32 `json:"vout"`
      AmountUnits   int64  `json:"amount_units"`
      Address       string `json:"address"`
      BlockHeight   int64  `json:"block_height"`
      Confirmations int64  `json:"confirmations"`
  }

  type ViewActivityItem struct {
      TxID              string `json:"txid"`
      Kind              string `json:"kind"`
      Status            string `json:"status"`
      NetUnits          int64  `json:"net_units"`
      BlockHeight       int64  `json:"block_height"`
      Confirmations     int64  `json:"confirmations"`
      BlocksUntilMature int64  `json:"blocks_until_mature"`
  }

  type ViewResponse struct {
      SchemaVersion        int                   `json:"schema_version"`
      Network              string                `json:"network"`
      Tip                  SnapshotTip           `json:"tip"`
      Addresses            []string              `json:"addresses"`
      Outputs              []SnapshotOutput      `json:"outputs"`
      SpendableUnits       int64                 `json:"spendable_units"`
      SpendableOutputCount int                   `json:"spendable_output_count"`
      ImmatureOutputs      []ViewImmatureOutput  `json:"immature_outputs"`
      ImmatureUnits        int64                 `json:"immature_units"`
      Activity             []ViewActivityItem    `json:"activity"`
  }
  ```

- [ ] Implement gateway conversion from `core.WalletViewForPKHs`.

  Register `ViewPath` beside the existing snapshot and broadcast routes. Reuse the gateway's method, content-type, body-size, canonical-address, timeout, and JSON-error rules.

- [ ] Implement `Client.View(ctx, addresses, activityLimit)` with full defensive validation.

  Keep `Client.Snapshot` untouched. Use one internal strict response validator for common tip/output rules only when that does not alter legacy behavior.

- [ ] Run focused tests, all lightwallet tests, and the old-client compatibility test.

  ```powershell
  go test ./lightwallet -run 'View|LegacySnapshot|Snapshot' -count=1
  go test ./lightwallet -count=1
  ```

  Expected: PASS.

- [ ] Commit the gateway view.

  ```powershell
  git add lightwallet/types.go lightwallet/gateway.go lightwallet/client.go lightwallet/view_test.go lightwallet/gateway_test.go lightwallet/client_test.go
  git commit -m "Add backward compatible wallet view endpoint"
  ```

### Task 3: Build exact-size same-address cleanup transactions

**Files:**
- Create: `wallet/consolidation.go`
- Create: `wallet/consolidation_test.go`
- Modify: `wallet/wallet.go`
- Reference: `wallet/payment.go`
- Reference: `wallet/remote.go`

- [ ] Add failing tests for local and remote cleanup construction.

  Cover:

  - only outputs controlled by one address are selected;
  - the address group with the most eligible outputs is selected, with canonical address as the tie-breaker;
  - outputs are selected smallest amount first, then transaction hash and output index;
  - at least two inputs are required;
  - selected input value must exceed the fee;
  - the single output pays `sum(inputs) - fee` back to the selected owner address;
  - the signed encoded transaction never exceeds `core.MaxSignedTxBytes`;
  - the largest fitting prefix is selected and `MoreAvailable` reports remaining eligible outputs;
  - excluded outpoints are not selected;
  - foreign-owner, changed-tip, locked-wallet, overflow, and invalid-remote-snapshot cases are rejected;
  - V1 and V2 wallets both work without mixing addresses;
  - local and remote builders produce the same selected outpoints and transaction ID for the same snapshot.

- [ ] Run the focused tests and confirm the cleanup API is missing.

  Run: `go test ./wallet -run Cleanup -count=1`

  Expected: FAIL with undefined cleanup types and methods.

- [ ] Implement the cleanup contracts.

  ```go
  var ErrNoCleanupNeeded = errors.New("no cleanup needed")
  var ErrCleanupTooSmall = errors.New("cleanup inputs do not cover fee")

  type PreparedCleanup struct {
      Tx                *core.Tx
      Address           string
      AmountUnits       int64
      FeeUnits          int64
      SelectedOutpoints []core.OutPoint
      MoreAvailable     bool
  }

  func (w *Wallet) PrepareCleanupAt(
      c *core.Chain,
      expected core.ChainTipSnapshot,
      fee int64,
      excluded map[core.OutPoint]struct{},
  ) (Snapshot, *PreparedCleanup, error)

  func (w *Wallet) PrepareCleanupFromRemoteSnapshot(
      remote RemoteSnapshot,
      fee int64,
      excluded map[core.OutPoint]struct{},
  ) (Snapshot, *PreparedCleanup, error)
  ```

- [ ] Implement one shared `buildCleanupFromSnapshot` path under the wallet lock.

  Build eligible groups by signing key, choose deterministically, maintain checked prefix sums, and use binary search over prefix length. Sign every size probe with the real transaction encoder and select the largest transaction for which `len(tx.Bytes()) <= core.MaxSignedTxBytes`.

- [ ] Revalidate the selected anchored outputs and expected tip before returning a local preview.

  The returned transaction must have one output, no change output, and every input signature must verify through the existing transaction validation path.

- [ ] Run focused and full wallet tests.

  ```powershell
  go test ./wallet -run 'Cleanup|Payment' -count=1
  go test ./wallet -count=1
  ```

  Expected: PASS.

- [ ] Commit cleanup construction.

  ```powershell
  git add wallet/consolidation.go wallet/consolidation_test.go wallet/wallet.go
  git commit -m "Build safe same address wallet cleanup transactions"
  ```

### Task 4: Add backend-calculated maximum send

**Files:**
- Create: `wallet/maximum.go`
- Create: `wallet/maximum_test.go`
- Modify: `wallet/wallet.go`

- [ ] Add failing local and remote maximum-send tests.

  Cover exact fee subtraction, excluded outputs, insufficient funds, invalid fee, foreign snapshot ownership, locked wallet, stale tip, and a transaction that exceeds the signed-size limit.

- [ ] Run the focused tests and confirm the methods are missing.

  Run: `go test ./wallet -run Maximum -count=1`

  Expected: FAIL with undefined maximum-send methods.

- [ ] Implement atomic maximum-send helpers.

  ```go
  func (w *Wallet) PrepareMaximumAt(
      c *core.Chain,
      expected core.ChainTipSnapshot,
      toAddress string,
      fee int64,
      excluded map[core.OutPoint]struct{},
  ) (Snapshot, *PreparedPayment, error)

  func (w *Wallet) PrepareMaximumFromRemoteSnapshot(
      remote RemoteSnapshot,
      toAddress string,
      fee int64,
      excluded map[core.OutPoint]struct{},
  ) (Snapshot, *PreparedPayment, error)
  ```

  Calculate `amount = spendableUnits - fee` inside the same locked snapshot used to build the payment. Reuse the normal payment selector and its size, signature, owner, and network checks.

- [ ] Run focused and full wallet tests.

  ```powershell
  go test ./wallet -run 'Maximum|Payment' -count=1
  go test ./wallet -count=1
  ```

  Expected: PASS.

- [ ] Commit maximum send.

  ```powershell
  git add wallet/maximum.go wallet/maximum_test.go wallet/wallet.go
  git commit -m "Add atomic maximum send previews"
  ```

### Task 5: Expose typed activity, Max, and cleanup operations through the app service

**Files:**
- Modify: `cmd/btc09/app_service.go`
- Modify: `cmd/btc09/app_service_test.go`
- Modify: `desktop/types.go`

- [ ] Extend the fake gateway and add failing service tests.

  Cover full-node and fast-mode parity for status, activity, maximum send, and cleanup. Assert:

  - status distinguishes spendable and immature units;
  - cleanup is available at two eligible same-address outputs whose sum exceeds the fee;
  - cleanup is recommended at 20 eligible same-address outputs;
  - activity is capped at 50;
  - cleanup preview never broadcasts;
  - send and cleanup confirmation tokens cannot be used on the other route;
  - replay, expiry, concurrent confirmation, remote rejection, and successful broadcast preserve existing safety behavior;
  - public errors use the approved short language.

- [ ] Run the focused test and confirm the service contracts are missing.

  Run: `go test ./cmd/btc09 -run 'AppService.*(Activity|Maximum|Cleanup|Status)' -count=1`

  Expected: FAIL with missing methods and response fields.

- [ ] Add the desktop data contracts in `desktop/types.go`.

  Add these fields to `Status`:

  ```go
  ImmatureUnits       int64 `json:"immature_units"`
  SpendableOutputCount int  `json:"spendable_output_count"`
  CleanupAvailable    bool  `json:"cleanup_available"`
  CleanupRecommended  bool  `json:"cleanup_recommended"`
  ```

  Add:

  ```go
  type MaxSendRequest struct {
      Destination string `json:"destination"`
      Fee         string `json:"fee"`
  }

  type CleanupRequest struct {
      Fee string `json:"fee"`
  }

  type CleanupPreview struct {
      PendingID       string `json:"pending_id"`
      Address         string `json:"address"`
      AmountUnits     int64  `json:"amount_units"`
      FeeUnits        int64  `json:"fee_units"`
      InputCount      int    `json:"input_count"`
      MoreAvailable   bool   `json:"more_available"`
      TxID            string `json:"txid"`
      ChainHeight     int64  `json:"chain_height"`
      ExpiresAtUnix   int64  `json:"expires_at_unix"`
      ConfirmationCode string `json:"confirmation_code"`
  }

  type ActivityItem struct {
      TxID              string `json:"txid"`
      Kind              string `json:"kind"`
      Status            string `json:"status"`
      NetUnits          int64  `json:"net_units"`
      BlockHeight       int64  `json:"block_height"`
      Confirmations     int64  `json:"confirmations"`
      BlocksUntilMature int64  `json:"blocks_until_mature"`
  }

  type ActivityResult struct {
      Height int64          `json:"height"`
      Items  []ActivityItem `json:"items"`
  }

  type WalletFeaturesService interface {
      Activity(context.Context) (ActivityResult, error)
      PreviewMaxSend(context.Context, MaxSendRequest) (SendPreview, error)
      PreviewCleanup(context.Context, CleanupRequest) (CleanupPreview, error)
      ConfirmCleanup(context.Context, string) (SendResult, error)
  }
  ```

- [ ] Add `View(context.Context, []string, int)` to `appGateway` and use activity limit zero for status/send previews and 50 for the Activity screen.

- [ ] Type app-service pending previews with `send` and `cleanup` purposes.

  `ConfirmSend` must reject cleanup previews and `ConfirmCleanup` must reject send previews before marking either preview in flight.

- [ ] Implement full-node and fast-mode conversions.

  Full-node mode reads `core.WalletViewForPKHs`; fast mode reads the new lightwallet view. Both modes must calculate cleanup availability per owner address and use the wallet package to construct transactions.

- [ ] Run focused and package tests.

  ```powershell
  go test ./cmd/btc09 -run 'AppService.*(Activity|Maximum|Cleanup|Status|Send)' -count=1
  go test ./cmd/btc09 -count=1
  ```

  Expected: PASS.

- [ ] Commit app-service features.

  ```powershell
  git add cmd/btc09/app_service.go cmd/btc09/app_service_test.go desktop/types.go
  git commit -m "Expose wallet activity and cleanup services"
  ```

### Task 6: Add authenticated desktop HTTP routes and typed pending sessions

**Files:**
- Create: `desktop/wallet_features.go`
- Create: `desktop/wallet_features_test.go`
- Modify: `desktop/server.go`
- Modify: `desktop/server_test.go`

- [ ] Add failing HTTP tests for all new routes.

  Routes:

  - `GET /api/v1/activity`
  - `POST /api/v1/send/max-preview`
  - `POST /api/v1/maintenance/cleanup/preview`
  - `POST /api/v1/maintenance/cleanup/confirm`

  Cover read authentication, method rejection, CSRF enforcement, strict JSON, body size, missing optional feature service, typed purpose mismatch, preview expiry, in-flight conflict, replay rejection, and service-error mapping.

- [ ] Run focused tests and confirm handlers are absent.

  Run: `go test ./desktop -run 'Activity|MaxPreview|Cleanup' -count=1`

  Expected: FAIL with 404 responses or missing test service methods.

- [ ] Register the routes and require the optional `WalletFeaturesService`.

  Return `501 Not Implemented` when the base service does not implement the feature interface. Keep all existing base service implementations source-compatible.

- [ ] Generalize the server's per-session pending record with a `purpose` field.

  A confirmation request must match the session, preview ID, purpose, expiry, and not-in-flight state before calling the app service.

- [ ] Reuse the existing strict request decoder and JSON response/error helpers.

  Activity uses the existing authenticated read policy. All three mutation routes use the existing origin, CSRF, and no-store protections.

- [ ] Run focused and full desktop tests.

  ```powershell
  go test ./desktop -run 'Activity|MaxPreview|Cleanup|Send' -count=1
  go test ./desktop -count=1
  ```

  Expected: PASS.

- [ ] Commit desktop routes.

  ```powershell
  git add desktop/wallet_features.go desktop/wallet_features_test.go desktop/server.go desktop/server_test.go
  git commit -m "Add desktop wallet feature endpoints"
  ```

### Task 7: Build the five-tab wallet interface and inspect it visually

**Files:**
- Modify: `desktop/assets/index.html`
- Modify: `desktop/assets/app.js`
- Modify: `desktop/assets/app.css`
- Modify: `desktop/assets_test.go`
- Create: `docs/superpowers/evidence/v0.1.32-desktop-wide.png`
- Create: `docs/superpowers/evidence/v0.1.32-desktop-narrow.png`

- [ ] Add failing asset tests for the new labels, controls, routes, and safety copy.

  Assert the embedded assets contain:

  - five tabs in this order: Receive, Send, Activity, Mine, Backup;
  - `READY TO SEND` and the mining-rewards secondary line;
  - the Max control and max-preview route;
  - Activity and cleanup routes;
  - explorer and copy controls for transaction IDs;
  - no private key, seed phrase, signed transaction, or raw script rendered into page markup.

- [ ] Run asset tests and confirm the UI contracts are absent.

  Run: `go test ./desktop -run Assets -count=1`

  Expected: FAIL on missing copy and route strings.

- [ ] Update the shell and balance card.

  Insert Activity between Send and Mine. Change `TOTAL BALANCE` to `READY TO SEND`, show `Mining rewards waiting` beneath it when nonzero, and keep the values compact enough for narrow screens.

- [ ] Add backend Max and clearer send review.

  The Max button calls `/api/v1/send/max-preview` and opens the existing review dialog with the calculated amount. Display `Uses N payments` when more than one input is selected. Keep a successful send result visible with copy and explorer controls.

- [ ] Build the Activity screen.

  Render at most 50 rows with labels `Received`, `Sent`, `Mining reward`, and `Wallet cleanup`; show `Waiting` or the confirmation count; show a signed amount; and provide copy/open controls for the transaction ID. Poll every 30 seconds only while Activity is selected.

- [ ] Build the wallet-tools cleanup card and confirmation dialog.

  Show the card when cleanup is available and elevate it without an alarm treatment when recommended. Preview must show payment count, fee, destination address, current height, confirmation code, and whether another pass may remain. Confirmation calls only the cleanup confirmation route.

  Use these user-facing errors:

  - `Nothing to combine yet.`
  - `Those payments are already being used by a pending transaction.`
  - `The wallet changed. Review the cleanup again.`
  - `This cleanup is too large for one transaction. Confirm this batch first, then run it again after it confirms.`

- [ ] Make the five-tab navigation and dialogs work at narrow widths.

  Preserve the current visual language. Avoid oversized headings, dense paragraphs, equal visual weight on every control, and decorative gradients that do not already belong to the product.

- [ ] Run asset and desktop tests.

  ```powershell
  go test ./desktop -run Assets -count=1
  go test ./desktop -count=1
  ```

  Expected: PASS.

- [ ] Run the desktop app locally with a disposable test wallet and inspect wide and narrow screenshots.

  Use the repository's existing local-app test mode. Capture one desktop-width screenshot and one phone-width screenshot with the Activity screen visible. Inspect both images for clipping, text scale, tab wrapping, dialog hierarchy, empty states, and accidental technical data.

- [ ] Correct every visible issue and repeat the screenshots and asset tests.

- [ ] Commit the UI and reviewed evidence.

  ```powershell
  git add desktop/assets/index.html desktop/assets/app.js desktop/assets/app.css desktop/assets_test.go docs/superpowers/evidence/v0.1.32-desktop-wide.png docs/superpowers/evidence/v0.1.32-desktop-narrow.png
  git commit -m "Add clear wallet activity and cleanup interface"
  ```

### Task 8: Add concise Discord wallet and mining help without duplicate posts

**Files:**
- Modify: `tools/discord/stats-bot.mjs`
- Modify: `tools/discord/stats-bot-cli.test.mjs`
- Modify: `bot/btc09_otc_bot.py`
- Modify: `bot/tests/test_discord_ui.py`
- Modify: `tools/discord/setup-server.mjs`
- Modify: `tools/discord/setup-server.test.mjs`

- [ ] Add failing Node tests for `/wallet` and `/mine` definitions, routing, and ephemeral responses.

  `/wallet` must point to the official release, give one short open-the-app instruction covering Windows, macOS, and Linux, and state that recovery words stay private. `/mine` must point users to the Mine tab, the official open-source CPU miner, and the official guide, and warn against binaries sent by direct message.

- [ ] Add failing Python tests showing the OTC bot preserves the two Node-owned command placeholders during guild bulk sync.

- [ ] Add failing setup tests proving existing `start-here` and `resources` marker posts are patched in place and not duplicated.

- [ ] Run the focused tests and confirm the commands are absent.

  ```powershell
  node --test tools/discord/stats-bot-cli.test.mjs tools/discord/setup-server.test.mjs
  python -m pytest bot/tests/test_discord_ui.py -q
  ```

  Expected: FAIL on missing wallet and mine definitions or content.

- [ ] Implement and export concise wallet/mining formatters in the Node stats bot.

  Add both definitions to `getCommandDefinitions`, both routes to `classifyInteraction`, and ephemeral replies to `handleInteraction`.

- [ ] Add `/wallet` and `/mine` placeholders to `register_node_owned_commands` in the Python OTC bot.

- [ ] Update existing seed content through the current marker IDs.

  Keep one marked message in each target channel. The setup script must locate the bot's existing marker message and use PATCH when the body changed.

- [ ] Run all Discord-focused tests.

  ```powershell
  node --test tools/discord/*.test.mjs
  python -m pytest bot/tests/test_discord_ui.py -q
  ```

  Expected: PASS.

- [ ] Commit Discord help.

  ```powershell
  git add tools/discord/stats-bot.mjs tools/discord/stats-bot-cli.test.mjs bot/btc09_otc_bot.py bot/tests/test_discord_ui.py tools/discord/setup-server.mjs tools/discord/setup-server.test.mjs
  git commit -m "Add simple Discord wallet and mining help"
  ```

### Task 9: Update version, website, documentation, and release contracts

**Files:**
- Modify: `cmd/btc09/main.go`
- Modify: `cmd/btc09/main_test.go`
- Modify: `README.md`
- Modify: `docs/index.html`
- Modify: `docs/EXCHANGE-INTEGRATION.md`
- Create: `docs/RELEASE-v0.1.32.md`
- Modify: `tools/site/test_index_contract.py`
- Modify: `tools/discord/setup-server.mjs`
- Modify: `tools/discord/setup-server.test.mjs`

- [ ] Add or update failing version and site-contract tests for v0.1.32.

  Assert the source version, website download links, supported-release references, release notes, and Discord resource post all agree on v0.1.32.

- [ ] Run focused tests and confirm they fail against v0.1.31.

  ```powershell
  go test ./cmd/btc09 -run Version -count=1
  python -m pytest tools/site/test_index_contract.py -q
  node --test tools/discord/setup-server.test.mjs
  ```

  Expected: FAIL on stale version and release references.

- [ ] Set `nodeVersion` to `v0.1.32` and update plain-language product copy.

  Website and README copy must describe:

  - Activity with transaction and mining-reward history;
  - Max send calculated by the wallet;
  - Combine small payments as an optional wallet tool;
  - no claim that cleanup increases balance or mining rewards;
  - no promise of exchange listing, price, or financial return.

- [ ] Write `docs/RELEASE-v0.1.32.md` with changes, upgrade steps, artifact matrix, checksums instructions, and the unchanged wallet-backup warning.

- [ ] Run the focused version, site, and Discord tests.

  ```powershell
  go test ./cmd/btc09 -run Version -count=1
  python -m pytest tools/site/test_index_contract.py -q
  node --test tools/discord/setup-server.test.mjs
  ```

  Expected: PASS.

- [ ] Commit release-facing content.

  ```powershell
  git add cmd/btc09/main.go cmd/btc09/main_test.go README.md docs/index.html docs/EXCHANGE-INTEGRATION.md docs/RELEASE-v0.1.32.md tools/site/test_index_contract.py tools/discord/setup-server.mjs tools/discord/setup-server.test.mjs
  git commit -m "Prepare BTC09 v0.1.32 release content"
  ```

### Task 10: Run the complete local quality and security gate

**Files:**
- Modify only files required to fix failures caused by this feature.

- [ ] Format all changed Go files and confirm the worktree diff is cleanly formed.

  ```powershell
  gofmt -w core/wallet_view.go core/wallet_view_test.go core/chain.go lightwallet/types.go lightwallet/gateway.go lightwallet/client.go lightwallet/view_test.go lightwallet/gateway_test.go lightwallet/client_test.go wallet/consolidation.go wallet/consolidation_test.go wallet/maximum.go wallet/maximum_test.go wallet/wallet.go cmd/btc09/app_service.go cmd/btc09/app_service_test.go cmd/btc09/main.go cmd/btc09/main_test.go desktop/types.go desktop/wallet_features.go desktop/wallet_features_test.go desktop/server.go desktop/server_test.go
  git diff --check
  ```

  Expected: no output from `git diff --check`.

- [ ] Run the complete Go test and vet gates.

  ```powershell
  go test ./... -count=1
  go vet ./...
  ```

  Expected: PASS with no vet findings.

- [ ] Run Python and Node suites.

  ```powershell
  python -m pytest bot/tests tools/site tools/deploy tools/ci -q
  node --test tools/discord/*.test.mjs
  ```

  Expected: all tests pass; existing explicitly skipped integration tests remain identified as skipped.

- [ ] Run the vulnerability scanner when installed.

  Run: `govulncheck ./...`

  Expected: no reachable known vulnerability. If the binary is unavailable, install the official `golang.org/x/vuln/cmd/govulncheck` tool at its current compatible version, then rerun.

- [ ] Run race-enabled Go tests on Linux.

  Use WSL or the staging workspace on the existing VPS:

  ```bash
  go test -race ./core ./wallet ./lightwallet ./desktop ./cmd/btc09 -count=1
  ```

  Expected: PASS with no race report.

- [ ] Review the complete diff against the approved design.

  Confirm every design requirement has code and test coverage, the snapshot response shape is untouched, typed confirmations cannot cross purposes, and no unrelated behavior or secret entered the diff.

- [ ] Commit any gate-driven corrections with focused messages, then rerun the complete gate.

### Task 11: Build and inspect release artifacts

**Files:**
- Create: `dist/v0.1.32/*`
- Modify only release scripts if a verified packaging defect requires correction.

- [ ] Build the supported artifact matrix from the clean commit.

  Build Windows amd64, Linux amd64, Linux arm64, macOS amd64, and macOS arm64. Package macOS artifacts with `tools/release/package_macos.py`. Include the required README, license, and launch instructions established by the current release process.

- [ ] Generate `SHA256SUMS` from the final archive bytes.

- [ ] Extract every archive into a new inspection directory and run each compatible binary with `version` or `--version`.

  Expected: every binary reports `v0.1.32` and the intended OS and architecture.

- [ ] Inspect archive entry lists and scan extracted files for forbidden material.

  Reject any artifact containing `.env`, wallet databases, recovery words, tokens, API keys, private keys, local logs, test fixtures with secrets, or unrelated repository files.

- [ ] Smoke-test the Windows app and at least one Linux artifact.

  Confirm startup, wallet unlock/create flow, status, activity, normal send preview, Max preview, cleanup preview, cancel, and explicit confirmation behavior.

- [ ] Record artifact names, byte sizes, and SHA-256 values in the release notes or release checklist.

### Task 12: Publish, deploy, synchronize Discord, and verify production

**Files:**
- No source edits unless live verification reveals a release-blocking defect.

- [ ] Push the feature branch and open a pull request containing the design, implementation, tests, compatibility note, visual evidence, and release checklist.

- [ ] Read back GitHub Actions results.

  Required repository checks must pass. If GitHub fails before a job starts because hosted runners are unavailable for the account, capture that exact platform result and rely only on the already-passing local and Linux gates for diagnosis; do not mislabel it as a code pass.

- [ ] Merge only after review and required checks are resolved.

- [ ] Create signed or annotated tag `v0.1.32`, create the GitHub release, upload all archives and `SHA256SUMS`, then read the release and asset list back through GitHub.

  Verify every expected asset exists once, reports a nonzero size, and matches the local checksum.

- [ ] Inspect the VPS before touching services.

  Run service and process discovery first:

  ```bash
  systemctl list-units --all 'btc09*'
  systemctl list-unit-files 'btc09*'
  ps -ef | grep '[b]tc09'
  ss -lntup
  ```

  Identify the actual node, explorer, wallet gateway, OTC bot, and stats bot units and their working directories. Do not assume unit names.

- [ ] Stage the verified Linux binary and required source/static files beside the live deployment, back up only the files being replaced, and atomically install them.

  Restart one affected service at a time. Read its status and recent logs before moving to the next service.

- [ ] Verify old and new wallet APIs against production.

  Required checks:

  - `/api/wallet/v1/view` returns canonical spendable, immature, and activity data;
  - the v0.1.31 client still decodes `/api/wallet/v1/snapshot` successfully;
  - malformed, oversized, wrong-method, and wrong-content-type requests are rejected;
  - public explorer TXID and address-history pages still work;
  - website downloads and checksum links resolve to v0.1.32.

- [ ] Synchronize Discord application commands and seed posts once.

  Verify guild commands contain the OTC command tree plus Node-owned `/stats`, `/rank`, `/leaderboard`, `/wallet`, and `/mine`; global commands remain absent; each marked seed post exists exactly once and contains the new official release links.

- [ ] Verify Discord services and commands live.

  Check service status and recent warning/error logs. Invoke `/trade`, `/stats`, `/wallet`, and `/mine` in the guild and confirm each completes, uses the intended visibility, and does not remain stuck on `thinking`.

- [ ] Post one short human release announcement only after all production checks pass.

  Mention Activity, Max, and Combine small payments. Link the official release and checksum. Do not mention private planning conversations, exchange-price speculation, or guarantees.

- [ ] Perform final live read-back.

  Recheck the GitHub release, website, public APIs, systemd services, Discord command list, updated seed posts, and the single announcement. Save exact deployed commit, tag, artifact checksums, service names, and verification timestamps in the handoff.

