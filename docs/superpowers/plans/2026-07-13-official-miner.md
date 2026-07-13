# BTC09 Official Miner Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Put a safe one-click remote-solo miner with live feedback inside the BTC09 desktop wallet and publish its open coordinator endpoint.

**Architecture:** Extend the existing open mining client with progress events and resilient retry, add an optional miner service to the desktop wallet, and expose the existing coordinator through a hardened HTTPS route. The payout address remains user-controlled and the coordinator never receives wallet secrets.

**Tech Stack:** Go 1.25, existing HTML/CSS/JavaScript desktop UI, existing BTC09 remote-solo JSON protocol, nginx, Cloudflare, systemd.

## Global Constraints

- User-facing mode name is `Open solo`; do not call it a pool or imply partial-share payouts.
- Private keys, wallet files, and seed material never leave the local app; only payout address and optional worker label reach the coordinator.
- Default thread count is `max(1, logical CPUs - 1)` and UI input never exceeds logical CPU count.
- No NTMminer binary, protocol reverse engineering, arbitrary miner download, automatic startup, GPU claim, profitability claim, or temperature claim.
- Existing Fast Wallet is still the default and wallet send/receive/backup behavior must not regress.
- Visible copy is plain English and human-sounding.

---

### Task 1: Observable and resilient open miner

**Files:**
- Modify: `pool/work.go`
- Modify: `pool/work_test.go`
- Modify: `pool/client.go`
- Modify: `pool/client_test.go`

**Interfaces:**
- Produces: `MineProgress`, `MineWorkWithProgress(ctx, work, params, workers, interval, callback)`, `ClientEvent`, `RemoteClient.RunWithEvents(ctx, callback)`, and existing `MineWork`/`Run` compatibility.

- [ ] Write failing tests for periodic progress, monotonic hashes, correct current/average H/s, cancellation, found-block final progress, and nil callback.
- [ ] Run focused work tests and verify failure because the progress API is absent.
- [ ] Implement progress reporting outside the nonce hot loop and pass focused tests.
- [ ] Write failing client tests for job/progress/accepted events, retryable transport/429/5xx failures, fatal invalid-work/4xx failures, bounded exponential backoff, and cancellation during backoff.
- [ ] Run focused client tests to verify failure, implement the event/retry loop, then run `go test -race ./pool -count=1` on Linux.
- [ ] Commit `feat: make the official miner observable and resilient`.

### Task 2: Desktop miner service and local API

**Files:**
- Modify: `desktop/types.go`
- Modify: `desktop/server.go`
- Modify: `desktop/server_test.go`
- Modify: `cmd/btc09/app_service.go`
- Modify: `cmd/btc09/app_service_test.go`
- Modify: `cmd/btc09/app.go`
- Modify: `cmd/btc09/app_test.go`

**Interfaces:**
- Produces: optional `desktop.MinerService`, `MinerStatus`, `MinerStartRequest`, `StartMiner`, `StopMiner`, and authenticated `/api/v1/miner/*` routes.

- [ ] Write failing app-service tests for missing wallet, automatic address, default and bounded workers, one active session, status snapshots, stop idempotence, accepted block accounting, retry state, and shutdown cancellation.
- [ ] Run focused tests and verify the miner service is absent.
- [ ] Implement one-session lifecycle with injected client factory and clock for real tests without mocks of business behavior.
- [ ] Write failing desktop server tests for status/start/stop auth, CSRF, strict JSON, unsupported services, and public error mapping.
- [ ] Implement the optional routes without changing the base `desktop.Service` interface, then pass all desktop and app-service tests.
- [ ] Commit `feat: add one-click mining service to the wallet`.

### Task 3: Miner interface

**Files:**
- Modify: `desktop/assets/index.html`
- Modify: `desktop/assets/app.css`
- Modify: `desktop/assets/app.js`
- Modify: `desktop/assets_test.go`

**Interfaces:**
- Consumes: `/api/v1/miner/status`, `/start`, and `/stop`.
- Produces: wallet Mining tab with setup, running, retry, stopped, unavailable, and error states.

- [ ] Add failing asset tests for the Open solo label, solo variance explanation, privacy copy, wallet-address behavior, worker slider bounds, exact metrics, start/stop controls, one-second active polling, reduced-motion support, and absence of pool/profit/GPU claims.
- [ ] Run desktop asset tests and verify the required UI is absent.
- [ ] Implement semantic HTML, balanced responsive CSS, and state-driven JavaScript using the existing authenticated API helper.
- [ ] Run asset/server tests and commit `feat: add official miner to the desktop wallet`.

### Task 4: Public coordinator deployment and protocol docs

**Files:**
- Create: `deploy/nginx/bitcoin09-open-miner-http.conf`
- Create: `deploy/nginx/bitcoin09-open-miner-server.conf`
- Create: `deploy/scripts/install-open-miner.sh`
- Modify: `deploy/README.md`
- Create: `docs/OPEN-MINING-PROTOCOL.md`
- Modify: `README.md`
- Modify: `QUICKSTART.md`

**Interfaces:**
- Produces: HTTPS work/submit origin backed by loopback `127.0.0.1:9010` and a complete protocol v1 reference.

- [ ] Add failing deployment contract tests for two POST-only routes, Cloudflare real-IP handling, body/connection/request limits, no wildcard upstream, and health-safe rollback.
- [ ] Implement nginx fragments and idempotent installer; run shell/static contract tests.
- [ ] Document request/response schemas, validation, threat boundary, canonical vectors, CLI example, and distinction from pooled shares; verify every JSON example against Go types.
- [ ] Commit `docs: publish and harden the open mining protocol`.

### Task 5: Release, live validation, and project surfaces

**Files:**
- Modify: `docs/index.html`
- Modify: `tools/site/test_index_contract.py`
- Modify: `tools/discord/setup-server.mjs`
- Modify: relevant Discord tests and release docs.

**Interfaces:**
- Produces: versioned binaries, live coordinator, website guidance, and one tested Discord announcement.

- [ ] Run formatting, `go test ./... -count=1`, `go vet ./...`, Linux race tests for pool/desktop/cmd, site/Discord tests, and `govulncheck ./...`.
- [ ] Build Windows, Linux amd64/arm64, and macOS binaries with exact VCS metadata and inspect hashes/assets.
- [ ] Visually inspect stopped, mining, retry, error, and accepted states at 1280x800 and 390x844; fix only with retained contract tests.
- [ ] Deploy the coordinator behind Cloudflare/nginx, verify loopback binding, work validation, wrong-network/tamper/rate/body rejection, accepted regtest path, and all existing live services.
- [ ] Publish the release and website, then read back GitHub assets, latest tag, public docs, coordinator behavior, and website version.
- [ ] Update Discord in plain English only after live success; apply once and verify exactly one announcement and current official-miner guidance.
