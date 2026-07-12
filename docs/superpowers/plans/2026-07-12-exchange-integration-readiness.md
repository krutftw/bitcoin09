# Exchange Integration Readiness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish a tested, operator-grade BTC09 deposit and withdrawal integration package and a truthful SafeTrade listing request.

**Architecture:** Keep the existing versioned explorer and strict machine-wallet commands as the integration boundary. Add a dependency-free read-only verifier that checks the public `/api/v1` contract, then document the full deposit and withdrawal flow with exact tip-pinning and reorg rules. Do not add custody, a public wallet RPC, or a new network service.

**Tech Stack:** Go 1.25 reference node, Python 3 standard library verifier and tests, Markdown operator documentation, GitHub releases.

## Global Constraints

- No third-party runtime dependencies.
- Never print private keys, wallet JSON, API credentials, or signed transaction hex in the smoke-test result.
- Public copy must be plain English, human-sounding, and contain no price promises or fake listing claims.
- The explorer remains bound to localhost behind nginx in production.
- SafeTrade submission happens only after release and live read-back verification.

---

### Task 1: Public exchange API smoke test

**Files:**
- Create: `tools/exchange/btc09_exchange_smoke.py`
- Create: `tools/exchange/test_btc09_exchange_smoke.py`

**Interfaces:**
- Consumes: `GET /api/v1/tip`, `GET /api/v1/block/{hash}`, and optional `GET /api/v1/address/{address}/outputs?expected_tip_hash={hash}&expected_tip_height={height}`.
- Produces: `check_exchange_api(base_url: str, address: str | None, timeout: float) -> dict[str, object]` and a one-line JSON CLI result.

- [ ] **Step 1: Write failing contract tests**

```python
def test_check_exchange_api_pins_optional_address_scan(self):
    result = smoke.check_exchange_api(self.base_url, VALID_ADDRESS, 2.0)
    self.assertTrue(result["ok"])
    self.assertEqual(result["network"], "btc09-mainnet")
    self.assertEqual(result["height"], 42)
    self.assertEqual(result["block_transactions"], 1)
    self.assertEqual(result["address_outputs"], 1)
    self.assertIn("expected_tip_hash=" + TIP_HASH, self.server.last_address_path)
    self.assertIn("expected_tip_height=42", self.server.last_address_path)

def test_check_exchange_api_rejects_noncanonical_tip(self):
    self.server.tip_payload["hash"] = TIP_HASH.upper()
    with self.assertRaisesRegex(smoke.CheckFailed, "tip hash"):
        smoke.check_exchange_api(self.base_url, None, 2.0)
```

- [ ] **Step 2: Run tests and verify the missing module failure**

Run: `python -m unittest tools.exchange.test_btc09_exchange_smoke -v`

Expected: FAIL because `tools.exchange.btc09_exchange_smoke` does not exist.

- [ ] **Step 3: Implement bounded HTTP and schema validation**

```python
MAX_RESPONSE_BYTES = 4 * 1024 * 1024
HASH_RE = re.compile(r"^[0-9a-f]{64}$")

def _get_json(url: str, timeout: float) -> dict[str, object]:
    request = urllib.request.Request(url, headers={"Accept": "application/json"})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        raw = response.read(MAX_RESPONSE_BYTES + 1)
    if len(raw) > MAX_RESPONSE_BYTES:
        raise CheckFailed("response exceeds 4 MiB")
    value = json.loads(raw)
    if not isinstance(value, dict):
        raise CheckFailed("response is not a JSON object")
    return value

def check_exchange_api(base_url: str, address: str | None, timeout: float) -> dict[str, object]:
    base = base_url.rstrip("/")
    tip = _get_json(base + "/api/v1/tip", timeout)
    if tip.get("schema_version") != 1 or tip.get("network") not in {"btc09-mainnet", "btc09-regtest"}:
        raise CheckFailed("unsupported tip schema or network")
    height, tip_hash = tip.get("height"), tip.get("hash")
    if not isinstance(height, int) or height < 0:
        raise CheckFailed("invalid tip height")
    if not isinstance(tip_hash, str) or not HASH_RE.fullmatch(tip_hash):
        raise CheckFailed("invalid tip hash")
    block = _get_json(base + "/api/v1/block/" + tip_hash, timeout)
    if block.get("canonical") is not True or block.get("height") != height:
        raise CheckFailed("tip block is not canonical at expected height")
    txids = block.get("transaction_ids")
    if not isinstance(txids, list):
        raise CheckFailed("tip block transaction_ids missing")
    result = {"ok": True, "schema_version": 1, "network": tip["network"], "height": height,
              "tip_hash": tip_hash, "block_transactions": len(txids)}
    if address:
        query = urllib.parse.urlencode({"expected_tip_hash": tip_hash, "expected_tip_height": height})
        outputs = _get_json(base + "/api/v1/address/" + urllib.parse.quote(address, safe="") + "/outputs?" + query, timeout)
        rows = outputs.get("outputs")
        if outputs.get("tip") != {"height": height, "hash": tip_hash} or not isinstance(rows, list):
            raise CheckFailed("address snapshot is not pinned to the requested tip")
        result["address_outputs"] = len(rows)
    return result
```

The CLI accepts `--base-url`, optional `--address`, and `--timeout`; it prints the result with `json.dumps(..., separators=(",", ":"), sort_keys=True)`. Failures print `{"ok":false,"error_code":"exchange_api_check_failed"}` to stdout and the human reason to stderr, then exit 1.

- [ ] **Step 4: Run the focused tests**

Run: `python -m unittest tools.exchange.test_btc09_exchange_smoke -v`

Expected: all smoke-test cases PASS, including oversized JSON, malformed JSON, noncanonical hashes, wrong network, tip mismatch, and optional address pinning.

- [ ] **Step 5: Run against production**

Run: `python tools/exchange/btc09_exchange_smoke.py --base-url https://explorer.btc09.org`

Expected: one JSON line with `"ok":true`, `"network":"btc09-mainnet"`, and the current height.

- [ ] **Step 6: Commit**

```bash
git add tools/exchange/btc09_exchange_smoke.py tools/exchange/test_btc09_exchange_smoke.py
git commit -m "Add exchange API readiness smoke test"
```

### Task 2: Operator integration guide

**Files:**
- Create: `docs/EXCHANGE-INTEGRATION.md`
- Modify: `README.md`
- Modify: `docs/EXCHANGE-LISTING.md`

**Interfaces:**
- Consumes: machine commands documented by `btc09 usage` and the versioned explorer endpoints verified in Task 1.
- Produces: one stable operator contract linked from the README and listing package.

- [ ] **Step 1: Record the live JSON contracts**

Run:

```bash
curl -fsS https://explorer.btc09.org/api/v1/tip
curl -fsS https://explorer.btc09.org/api/v1/block/$(curl -fsS https://explorer.btc09.org/api/v1/tip | jq -r .hash)
```

Expected: schema version 1, network `btc09-mainnet`, and a canonical tip block with the same hash and height.

- [ ] **Step 2: Write the integration guide**

The guide must contain these exact top-level sections and complete commands, with no placeholder values except clearly named shell variables:

```markdown
## Support boundary
## Release verification
## Node and wallet layout
## Versioned chain API
## Deposit address allocation
## Tip-pinned deposit scanning
## Confirmations and reorg handling
## Withdrawal preparation and broadcast
## Backups and hot-wallet controls
## Read-only integration smoke test
## Incident recovery
```

Use a dedicated wallet path such as `/var/lib/btc09-exchange/hot-wallet.json`, mode `0600`, and a localhost explorer such as `127.0.0.1:8009`. Document these exact machine flows:

```bash
btc09 wallet new -wallet-file "$WALLET" -network btc09-mainnet -json
btc09 wallet snapshot -wallet-file "$WALLET" -datadir "$DATADIR" -network btc09-mainnet -expected-tip-hash "$TIP_HASH" -expected-tip-height "$TIP_HEIGHT" -json
printf '%s' "$EXCLUDED_OUTPOINTS_JSON" | btc09 prepare-send -to "$DESTINATION" -amount "$AMOUNT" -fee "$FEE" -datadir "$DATADIR" -network btc09-mainnet -wallet-file "$WALLET" -expected-tip-hash "$TIP_HASH" -expected-tip-height "$TIP_HEIGHT" -exclude-outpoints-json - -json
printf '%s' "$SIGNED_TX_HEX" | btc09 inspect-tx -tx-hex - -network btc09-mainnet -json
printf '%s' "$SIGNED_TX_HEX" | btc09 broadcast-tx -tx-hex - -expected-txid "$TXID" -datadir "$DATADIR" -network btc09-mainnet -seeds seed.btc09.org:9009 -json -require-broadcast=true
```

Recommend 30 confirmations at initial listing, explain that the exchange owns its final policy, require coinbase maturity of 100 blocks, and require rescanning from the last finalized height after any expected-tip conflict.

- [ ] **Step 3: Correct public claims and links**

Replace the listing spec's claim that wallet commands are self-evidently sufficient with a link to `EXCHANGE-INTEGRATION.md`. Add the versioned `/api/v1` endpoints and smoke-test command. Remove unverified current exchange submission URLs except SafeTrade's official support request route; keep other venues in a later-targets paragraph that requires live requirement checks.

Add a short `Exchange integration` section to `README.md` linking the guide. Do not claim BTC09 is listed.

- [ ] **Step 4: Validate documentation references**

Run: `rg -n "EXCHANGE-INTEGRATION|api/v1|btc09_exchange_smoke|30 confirmations|100 blocks" README.md docs/EXCHANGE-LISTING.md docs/EXCHANGE-INTEGRATION.md`

Expected: every promised interface appears in the integration guide, and the README and listing spec link it.

- [ ] **Step 5: Commit**

```bash
git add README.md docs/EXCHANGE-LISTING.md docs/EXCHANGE-INTEGRATION.md
git commit -m "Document BTC09 exchange integration contract"
```

### Task 3: SafeTrade submission package

**Files:**
- Create: `docs/SAFETRADE-LISTING-REQUEST.md`
- Modify: `BOOTSTRAP.md`

**Interfaces:**
- Consumes: verified release, source, explorer, integration guide, smoke test, Discord, Bitcointalk ANN, and fair-launch facts.
- Produces: a short request ready to paste into SafeTrade's official support form after release verification.

- [ ] **Step 1: Write the request**

The subject is `Bitcoin 09 (09C) listing request` and the body is 180-260 words. It must state: native UTXO chain, Argon2id 64 MiB proof of work, no premine/ICO/team allocation, 21M cap, live mainnet, current release, public source, explorer, integration guide, checksums, Discord, and maintainer contact through the support ticket. It must not use “highly anticipated,” “revolutionary,” price language, projected volume, or a claim that SafeTrade has approved anything.

- [ ] **Step 2: Reconcile the bootstrap strategy**

Replace `no exchange push` in `BOOTSTRAP.md` with `no exchange spam or paid listing campaign`. Add selective exchange submission after public integration and reliability checks. Keep `no paid influencers` and `no price talk as the pitch`.

- [ ] **Step 3: Run the full local gate**

Run:

```bash
go test ./...
python -m unittest discover -s bot/tests -v
python -m unittest tools.exchange.test_btc09_exchange_smoke -v
git diff --check
```

Expected: Go PASS; Python PASS with the repository's documented skips only; no whitespace errors.

- [ ] **Step 4: Commit**

```bash
git add BOOTSTRAP.md docs/SAFETRADE-LISTING-REQUEST.md
git commit -m "Prepare SafeTrade listing request"
```

### Task 4: Release and external submission gate

**Files:**
- Modify: `cmd/btc09/main.go` only if the release version is bumped under the repository's normal release process.
- Verify: GitHub release assets and public endpoints.

**Interfaces:**
- Consumes: Tasks 1-3 and the repository release workflow.
- Produces: a read-back verified release and a support-form submission receipt.

- [ ] **Step 1: Build and inspect release targets**

Run the repository's existing release workflow for Linux amd64/arm64, macOS amd64/arm64, and Windows amd64. Generate `SHA256SUMS.txt`, inspect every filename and digest, and scan the package for `.env`, wallet, database, token, and private-key files.

- [ ] **Step 2: Publish and read back GitHub**

After tests pass, push commits and publish the release. Read back the tag, asset list, sizes, and SHA-256 digests through GitHub. Do not call it shipped before this read-back succeeds.

- [ ] **Step 3: Deploy and verify public docs**

Verify `https://btc09.org`, `https://explorer.btc09.org/api/v1/tip`, the integration-guide GitHub URL, and the production smoke test. Check that ports 80, 443, and 9009 are reachable and explorer backend port 8009 remains externally blocked.

- [ ] **Step 4: Submit the SafeTrade request**

Open `https://support.safetrade.com/hc/en-us/requests/new`, enter the user's email, the prepared subject and body, and attach no secrets. Immediately before clicking `Submit`, obtain action-time confirmation if the browser confirmation policy requires it. Record the visible receipt or ticket number after submission.

