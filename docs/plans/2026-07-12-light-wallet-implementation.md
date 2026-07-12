# BTC09 Light Wallet Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Ship a non-custodial Fast mode that reads wallet state from an HTTPS VPS gateway and relays locally signed payments, while retaining Full node mode.

**Architecture:** Add a bounded `lightwallet` gateway/client package around atomic chain snapshots and signed transaction relay. Extend the wallet package to validate and sign from a remote snapshot, then make the desktop app select Fast mode by default on mainnet and retain its current local-chain implementation behind Full node mode.

**Tech Stack:** Go standard library HTTP/JSON, existing BTC09 core/wallet/P2P packages, embedded HTML/CSS/JavaScript, nginx/Cloudflare for the public HTTPS boundary.

---

### Task 1: Define and serve the bounded gateway snapshot contract

**Files:**
- Create: `lightwallet/types.go`
- Create: `lightwallet/gateway.go`
- Create: `lightwallet/gateway_test.go`

**Step 1: Write the failing tests**

Add table tests that require `POST /api/wallet/v1/snapshot` to accept a canonical list of owned addresses and return one atomic network/tip plus sorted mature unspent outputs. Require strict JSON, content type, no query string, canonical addresses, no duplicates, a 100-address cap, a bounded body, stable error codes, and POST-only routing.

**Step 2: Run the tests to verify RED**

Run: `go test ./lightwallet -run 'TestGatewaySnapshot|TestGatewayRequestBounds' -count=1`

Expected: FAIL because the package and handler do not exist.

**Step 3: Implement the minimal handler**

Define schema-versioned request/response types. Decode addresses to PKHs, call `SpendableOutputsForPKHs` once, verify the returned network/owners/order/money ranges, and serialize only public outpoint data. Keep the handler independent of nginx and bind policy.

**Step 4: Run the tests to verify GREEN**

Run: `go test ./lightwallet -run 'TestGatewaySnapshot|TestGatewayRequestBounds' -count=1`

Expected: PASS.

**Step 5: Commit**

Commit: `feat: add bounded light wallet snapshot gateway`

### Task 2: Add canonical signed-transaction relay

**Files:**
- Modify: `lightwallet/types.go`
- Modify: `lightwallet/gateway.go`
- Modify: `lightwallet/gateway_test.go`

**Step 1: Write the failing tests**

Require `POST /api/wallet/v1/broadcast` to accept lowercase canonical transaction hex plus the expected txid, reject malformed, oversized, uppercase, trailing, coinbase, mismatched, and invalid transactions, admit valid transactions atomically, relay only admitted or already-known valid transactions, and return an idempotent result.

**Step 2: Run the tests to verify RED**

Run: `go test ./lightwallet -run 'TestGatewayBroadcast' -count=1`

Expected: FAIL because the broadcast route is missing.

**Step 3: Implement the minimal relay**

Use `core.DecodeTx`, require exact re-encoding, compare the txid, call `AcceptTxWithResult`, and then call the injected peer broadcaster. Keep body and transaction sizes at or below the existing wallet transaction bound.

**Step 4: Run the tests to verify GREEN**

Run: `go test ./lightwallet -run 'TestGatewayBroadcast' -count=1`

Expected: PASS.

**Step 5: Commit**

Commit: `feat: relay signed light wallet transactions`

### Task 3: Build a hostile-input-safe light wallet client

**Files:**
- Create: `lightwallet/client.go`
- Create: `lightwallet/client_test.go`
- Modify: `wallet/wallet.go`
- Modify: `wallet/wallet_test.go`

**Step 1: Write the failing tests**

Require the client to enforce HTTPS on mainnet, bounded timeouts and responses, exact network identity, canonical tip/hash/address/outpoint formats, unique owned outputs, strict order, and money ranges. Require the wallet to sign from a validated remote snapshot while holding its wallet lock and to reject a server-supplied output not owned by the wallet.

**Step 2: Run the tests to verify RED**

Run: `go test ./lightwallet ./wallet -run 'TestClient|TestPrepareFromSnapshot' -count=1`

Expected: FAIL because the client and public snapshot preparation boundary do not exist.

**Step 3: Implement minimal validation and signing**

Create a client with injected `http.Client`, base URL, network, request/response limits, `Snapshot`, `Broadcast`, and `Transaction` operations. Add a wallet method that converts only a fully validated public snapshot into the existing deterministic snapshot representation and calls the existing local signing path.

**Step 4: Run the tests to verify GREEN**

Run: `go test ./lightwallet ./wallet -run 'TestClient|TestPrepareFromSnapshot' -count=1`

Expected: PASS.

**Step 5: Commit**

Commit: `feat: validate and sign remote wallet snapshots locally`

### Task 4: Wire Fast and Full node modes into the desktop service

**Files:**
- Modify: `cmd/btc09/app.go`
- Modify: `cmd/btc09/app_test.go`
- Modify: `cmd/btc09/app_service.go`
- Modify: `cmd/btc09/app_service_test.go`
- Modify: `desktop/types.go`
- Modify: `desktop/server_test.go`

**Step 1: Write the failing tests**

Require mainnet to default to Fast mode and the official HTTPS endpoint, allow explicit `-mode fast|full` and `-gateway URL`, reject insecure mainnet gateways, avoid starting P2P in Fast mode, show remote height/balance, prepare locally from remote outputs, relay signed bytes remotely, retain retryable previews, and keep the current Full node behavior.

**Step 2: Run the tests to verify RED**

Run: `go test ./cmd/btc09 ./desktop -run 'TestApp.*Mode|TestAppService.*Fast|TestStatus.*Mode' -count=1`

Expected: FAIL because mode fields and remote dependencies are missing.

**Step 3: Implement the mode boundary**

Add a narrow wallet-backend interface to the app service. Implement remote and local backends with identical snapshot/prepare/submit semantics. Start the P2P node and chain persistence only for Full mode. Fast mode opens the local wallet and gateway client only.

**Step 4: Run the tests to verify GREEN**

Run: `go test ./cmd/btc09 ./desktop -run 'TestApp.*Mode|TestAppService.*Fast|TestStatus.*Mode' -count=1`

Expected: PASS.

**Step 5: Commit**

Commit: `feat: make desktop wallet fast by default`

### Task 5: Refine the wallet UI for mode and transaction state

**Files:**
- Modify: `desktop/assets/index.html`
- Modify: `desktop/assets/app.css`
- Modify: `desktop/assets/app.js`
- Modify: `desktop/assets_test.go`

**Step 1: Write the failing contract tests**

Require a compact Fast/Full status control, safe service-error language, last block, pending/submitted payment status, and no implication that private keys are uploaded. Keep receive and send as the primary actions.

**Step 2: Run the tests to verify RED**

Run: `go test ./desktop -run 'TestEmbedded.*Fast|TestEmbedded.*Mode' -count=1`

Expected: FAIL because the new mode UI is absent.

**Step 3: Implement and visually refine**

Update only the compact account/status surfaces. Preserve the deliberate dark industrial wallet direction, typographic scale, responsive action layout, and current receive/send dialogs. Add restrained state transitions and accessible labels.

**Step 4: Run tests and inspect the real UI**

Run: `go test ./desktop -count=1`

Then build and inspect onboarding, loaded wallet, receive, send, offline, and submitted states at 1280x800 and 390x844 in a real browser.

Expected: tests PASS and no overflow, clipped controls, illegible copy, or generic landing-page styling.

**Step 5: Commit**

Commit: `feat: show fast wallet connectivity and payment state`

### Task 6: Expose the gateway from the node and verify the release

**Files:**
- Modify: `cmd/btc09/main.go`
- Modify: `cmd/btc09/main_test.go`
- Modify: `README.md`
- Modify: `docs/RELEASE-v0.1.24.md`
- Modify: website and Discord release copy after all gates pass

**Step 1: Write the failing node configuration tests**

Require a loopback wallet-gateway listener, clean shutdown, and refusal of unsafe public bind addresses unless explicitly overridden. Confirm the gateway shares the running node's canonical chain and peer broadcaster.

**Step 2: Run the tests to verify RED**

Run: `go test ./cmd/btc09 -run 'TestWalletGateway' -count=1`

Expected: FAIL because the node flag and server lifecycle are missing.

**Step 3: Implement, document, and version**

Add the node listener, release notes, operator instructions, nginx rate-limit/path configuration, and v0.1.24 version metadata. Do not expose the dedicated seed HTTP surface.

**Step 4: Run all verification gates**

Run: `gofmt` on changed Go files, `go test ./... -count=1`, `go vet ./...`, affected-package race tests on Linux, `govulncheck ./...`, cross-platform release builds, binary version inspection, and browser checks. Inspect release archives and SHA256 manifests.

Expected: all commands exit 0 with no test failures, vet findings, reachable vulnerabilities, modified build metadata, or hash mismatches.

**Step 5: Deploy and read back**

Deploy the gateway to `178.128.105.41` on loopback, route only `/api/wallet/v1/` through nginx/Cloudflare with strict rate and body limits, then verify public snapshot/broadcast rejection behavior, node health, explorer health, OTC health, P2P peers, and wallet Fast mode against HTTPS. Publish GitHub v0.1.24, update the website and Discord in plain human wording, and read each public destination back before claiming completion.

**Step 6: Commit and merge**

Commit: `release: ship BTC09 v0.1.24 light wallet`
