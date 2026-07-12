# Open Solo Mining Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a versioned, self-hostable remote-solo mining coordinator and open-source client while making Discord stats independent of third-party pool uptime.

**Architecture:** The existing node optionally serves a bounded HTTP mining API. It creates canonical block templates and stores opaque, short-lived jobs; clients receive only the 88-byte header and can submit only a nonce for a server-owned job. The `mine-pool` client performs Argon2id locally and sends only network-winning work, so the coordinator cannot be used as a low-difficulty hash-verification amplifier.

**Tech Stack:** Go standard library, existing `core` and `p2p` packages, Node.js built-in test runner for Discord tooling.

---

### Task 1: Define canonical mining work and local hashing

**Files:**
- Create: `pool/work.go`
- Test: `pool/work_test.go`

**Step 1: Write the failing tests**

Cover exact 88-byte header parsing, rejection of non-88-byte headers, rejection of a noncanonical network identity, and a regtest work item whose discovered nonce meets the encoded target.

**Step 2: Run tests to verify they fail**

Run: `go test ./pool -run 'Test(ParseWork|MineWork)' -count=1`

Expected: FAIL because package `pool` and its work parser do not exist.

**Step 3: Implement the minimal work model**

Add public response fields for `schema_version`, `network`, `job_id`, `height`, `header_hex`, `target_hex`, `expires_at`, and Argon2 parameters. Parse the header with explicit little-endian offsets and validate that the compact bits expand to the advertised 32-byte target. Add a cancellable worker loop that modifies only bytes 80 through 87 of a private header copy.

**Step 4: Run tests to verify they pass**

Run: `go test ./pool -run 'Test(ParseWork|MineWork)' -count=1`

Expected: PASS.

**Step 5: Commit**

Commit: `feat: add canonical remote mining work`

### Task 2: Build the bounded solo coordinator

**Files:**
- Create: `pool/coordinator.go`
- Test: `pool/coordinator_test.go`

**Step 1: Write the failing tests**

Specify that `Issue` rejects invalid addresses and worker labels, returns a canonical template paying the requested address, bounds stored jobs, and expires old jobs. Specify that `Submit` rejects unknown, expired, stale-tip, duplicate, and low-difficulty nonces before accepting a valid regtest block once.

**Step 2: Run tests to verify they fail**

Run: `go test ./pool -run 'TestCoordinator' -count=1`

Expected: FAIL because `Coordinator` is undefined.

**Step 3: Implement minimal coordinator behavior**

Use `crypto/rand` 128-bit job IDs, a mutex-protected job map capped at 256 entries, a two-minute default TTL, `core.BuildBlockTemplate`, and `core.Chain.AcceptBlock`. Store a deep copy of each template, reconstruct only the nonce on submission, compare the current tip before hashing, and mark a nonce used before expensive validation so duplicate concurrent submissions cannot multiply Argon2 work.

**Step 4: Run tests to verify they pass**

Run: `go test ./pool -run 'TestCoordinator' -count=1`

Expected: PASS.

**Step 5: Commit**

Commit: `feat: add bounded solo mining coordinator`

### Task 3: Expose a hardened versioned HTTP API

**Files:**
- Create: `pool/http.go`
- Test: `pool/http_test.go`

**Step 1: Write the failing tests**

Exercise `POST /api/v1/work`, `POST /api/v1/submit`, method rejection, strict JSON decoding, 4 KiB request limits, safe machine-readable errors, no-store headers, and per-IP request limits. Assert that responses never echo arbitrary worker input or internal errors.

**Step 2: Run tests to verify they fail**

Run: `go test ./pool -run 'TestHTTP' -count=1`

Expected: FAIL because the handler is undefined.

**Step 3: Implement the HTTP handler**

Use only `net/http`, `http.MaxBytesReader`, `json.Decoder.DisallowUnknownFields`, explicit content-type checking, constant-size error objects, and an in-memory fixed-window limiter. Set server read-header, read, write, and idle timeouts in the constructor used by the CLI.

**Step 4: Run tests to verify they pass**

Run: `go test ./pool -run 'TestHTTP' -count=1`

Expected: PASS.

**Step 5: Commit**

Commit: `feat: expose remote solo mining API`

### Task 4: Add the open miner client and CLI integration

**Files:**
- Create: `pool/client.go`
- Test: `pool/client_test.go`
- Modify: `cmd/btc09/main.go`
- Modify: `cmd/btc09/main_test.go`

**Step 1: Write the failing tests**

Use an `httptest` coordinator to prove the client requests work for the configured address, mines regtest work, submits only a winning nonce, refreshes expired work, refuses plain HTTP unless `-allow-insecure-http` is explicit, and never accepts a server-selected network different from `-network`.

**Step 2: Run tests to verify they fail**

Run: `go test ./pool ./cmd/btc09 -run 'Test(RemoteMiner|MinePool)' -count=1`

Expected: FAIL because the client and `mine-pool` command do not exist.

**Step 3: Implement the minimal client and command**

Add `btc09 mine-pool -pool URL -address ADDRESS [-worker NAME] [-workers N] [-network mainnet|regtest]`. Default to HTTPS, use bounded request/response sizes and timeouts, log hashrate locally, and keep payout identity entirely client-selected. Add `btc09 node -solo-api ADDRESS` to serve the coordinator using the already-synced node and normal P2P announcement hook.

**Step 4: Run tests to verify they pass**

Run: `go test ./pool ./cmd/btc09 -run 'Test(RemoteMiner|MinePool)' -count=1`

Expected: PASS.

**Step 5: Commit**

Commit: `feat: add open remote solo miner`

### Task 5: Remove the Discord bot's hard dependency on a pool API

**Files:**
- Modify: `tools/discord/stats-bot.mjs`
- Modify: `tools/discord/stats-bot-cli.test.mjs`

**Step 1: Write the failing tests**

Add a test where the explorer status succeeds and every third-party pool request fails. Assert that a useful chain-only message is produced with height, peers, estimated hashrate, retarget progress, and last-100-block payout concentration. Add a separate test proving optional pool data is clearly labeled when available.

**Step 2: Run tests to verify they fail**

Run: `node --test tools/discord/stats-bot-cli.test.mjs`

Expected: FAIL because `getStats` currently rejects when the pool is unavailable.

**Step 3: Implement the fallback**

Fetch the explorer as required and third-party endpoints with `Promise.allSettled`. Format the official chain section first and append the optional community-pool section only when its data is internally complete. Update the endpoint from the dead tutuit URL to the currently documented NTMminer URL without making it authoritative.

**Step 4: Run tests to verify they pass**

Run: `node --test tools/discord/stats-bot-cli.test.mjs`

Expected: PASS.

**Step 5: Commit**

Commit: `fix: keep Discord stats independent of pool uptime`

### Task 6: Document, stress-test, and integrate

**Files:**
- Modify: `README.md`
- Modify: `QUICKSTART.md`
- Modify: `docs/index.html`
- Create: `deploy/nginx/btc09-solo-mining.conf`
- Create: `deploy/systemd/btc09-seed-only.service`

**Step 1: Write or update contract tests first**

Add site assertions for the open-source remote solo path, the warning that it does not smooth solo variance, and the fact that PPLNS is not yet live.

**Step 2: Run tests to verify the documentation contract fails**

Run: `python -m unittest tools.site.test_index_contract`

Expected: FAIL on the missing open-mining copy.

**Step 3: Add operator and miner documentation**

Document TLS, firewall separation, API bind defaults, address-only authentication, independent coordinator operation, and limitations. Do not advertise the main project coordinator until it has passed live regtest validation.

**Step 4: Run full verification**

Run: `go test -race ./...`

Run: `node --test tools/discord/*.test.mjs`

Run: `python -m unittest tools.site.test_index_contract`

Expected: all PASS.

**Step 5: Commit**

Commit: `docs: publish open solo mining operator path`
