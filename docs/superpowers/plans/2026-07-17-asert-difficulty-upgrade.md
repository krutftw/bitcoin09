# ASERT Difficulty Upgrade Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace BTC09's cliff-edge 2,016-block retarget with a safe per-block ASERT rule at mainnet height 12,096 and ship the mandatory v0.1.34 upgrade before activation.

**Architecture:** Historical heights continue through the existing legacy `NextBits` path. Heights at or above the configured activation use one dynamically selected branch-local anchor and the published integer aserti3 calculation; contextual timestamp checks switch to MTP-11 plus a 30-minute future bound at the same height. The explorer, miner surfaces, releases, official nodes, website, and Discord all consume those canonical parameters.

**Tech Stack:** Go 1.25.12, `math/big`, Argon2id proof of work, Go tests, Node.js contract tests, Tauri 2.11.4, AppVeyor, systemd, GitHub Releases.

## Global Constraints

- Mainnet ASERT activation height is exactly `12096`.
- The ASERT anchor is block `12095`; its parent timestamp comes from block `12094` on the same branch.
- Target spacing remains `600` seconds and ASERT half-life is exactly `7200` seconds.
- Post-activation timestamps must exceed MTP-11 and must not exceed local time plus `1800` seconds.
- Blocks below height `12096` must validate byte-for-byte under the existing legacy rules.
- Consensus arithmetic contains no floating point.
- The target never exceeds `Params.MaxTarget()` and never becomes zero.
- Version `v0.1.34` is a mandatory upgrade; unsigned Windows artifacts are not published.

---

### Task 1: Consensus parameters and integer ASERT primitive

**Files:**
- Modify: `core/params.go`
- Modify: `core/pow.go`
- Create: `core/asert_test.go`

**Interfaces:**
- Consumes: `CompactToTarget(uint32) *big.Int`, `TargetToCompact(*big.Int) uint32`.
- Produces: `calculateASERTTarget(refTarget *big.Int, targetSpacing, timeDiff, heightDiff, halfLife int64, maxTarget *big.Int) *big.Int` and new `Params` fields `ASERTActivationHeight`, `ASERTHalfLife`, `ASERTFutureDrift`, `ASERTMedianTimeBlocks`.

- [ ] **Step 1: Write failing parameter and vector tests**

Add tests that assert canonical mainnet values `12096`, `7200`, `1800`, and `11`, regtest ASERT disabled, plus the published vector:

```go
func TestCalculateASERTMatchesPublishedVector(t *testing.T) {
    maxTarget := CompactToTarget(0x1d00ffff)
    got := calculateASERTTarget(
        CompactToTarget(0x1802aee8), 600,
        1234568190-1234567290, 1, 172800, maxTarget,
    )
    if bits := TargetToCompact(got); bits != 0x1802ae16 {
        t.Fatalf("ASERT bits = %08x, want 1802ae16", bits)
    }
}
```

- [ ] **Step 2: Run the focused tests and observe the missing symbols**

Run: `go test ./core -run 'Test(MainNetASERTParameters|CalculateASERT)' -count=1`

Expected: compile failure naming the new fields or `calculateASERTTarget`.

- [ ] **Step 3: Add parameters and the exact integer calculation**

Add these mainnet values and zero values for canonical regtest:

```go
ASERTActivationHeight: 12096,
ASERTHalfLife:          2 * 3600,
ASERTFutureDrift:       30 * 60,
ASERTMedianTimeBlocks:  11,
```

Implement exponent scaling by `65536`, floor decomposition into `shifts` and a `0..65535` fractional part, the published coefficients `195766423245049`, `971821376`, and `5127`, target shifting, and zero/pow-limit clamps using `big.Int`.

- [ ] **Step 4: Run primitive tests**

Run: `go test ./core -run 'Test(MainNetASERTParameters|CalculateASERT)' -count=1`

Expected: PASS for steady schedule, faster schedule, slower schedule, hardest-target clamp, pow-limit clamp, negative exponent decomposition, and the published vectors.

- [ ] **Step 5: Commit**

```powershell
git add core/params.go core/pow.go core/asert_test.go
git commit -m "Add integer ASERT consensus primitive"
```

### Task 2: Height activation, branch anchors, and timestamps

**Files:**
- Modify: `core/pow.go`
- Modify: `core/chain.go`
- Modify: `core/block.go`
- Modify: `core/asert_test.go`
- Modify: `core/core_test.go`
- Modify: `core/store_test.go`

**Interfaces:**
- Consumes: `calculateASERTTarget`, legacy `NextBits`, block-index ancestry.
- Produces: height-selecting `NextBits`, `asertActive(*Params, int64) bool`, contextual MTP validation, and replay-safe canonical/side-branch validation.

- [ ] **Step 1: Write activation and timestamp tests**

Cover all of these exact outcomes:

```go
// height 12095 uses legacy bits; height 12096 uses ASERT.
// First ASERT bits use bits(12095), time(12094), and time(12095).
// A side branch uses its own blocks 12094 and 12095.
// At activation, timestamp == MTP-11 is rejected and MTP-11+1 is accepted.
// At activation, now+1800 is accepted and now+1801 is rejected.
// Pre-activation parent-minus-FutureDrift behavior is unchanged.
// A legacy four-times-retarget block is rejected at height 12096.
```

- [ ] **Step 2: Run the focused tests and confirm red**

Run: `go test ./core -run 'Test(ASERTActivation|ASERTBranch|PostASERTTimestamp|LegacyTimestamp)' -count=1`

Expected: FAIL because `NextBits` still selects only the legacy window and contextual MTP is absent.

- [ ] **Step 3: Select ASERT by height**

In `NextBits`, retain the genesis case, then select ASERT only when activation is configured and `height >= ASERTActivationHeight`. Use:

```go
anchorHeight := p.ASERTActivationHeight - 1
parentHeight := height - 1
refTarget := CompactToTarget(bitsAt(anchorHeight))
target := calculateASERTTarget(
    refTarget,
    p.TargetBlockTime,
    timeAt(parentHeight)-timeAt(anchorHeight-1),
    parentHeight-anchorHeight,
    p.ASERTHalfLife,
    p.MaxTarget(),
)
return TargetToCompact(target)
```

- [ ] **Step 4: Add contextual timestamp enforcement**

Move the contextual check until after the parent-derived height is known. For ASERT heights, sort timestamps from the parent and up to ten ancestors, select `times[len(times)/2]`, require `candidate.Time > median`, and apply `ASERTFutureDrift`. For earlier heights, retain the existing future and parent-minus-drift checks exactly.

- [ ] **Step 5: Keep canonical replay O(1)**

When `parent == c.tip`, have `bitsOnBranch` use `nextBitsAtLocked(height)` so replay reads the main-chain arrays directly. Keep the ancestry walker for a real side branch and test that each callback resolves on that branch.

- [ ] **Step 6: Run focused and package tests**

Run: `go test ./core -count=1`

Expected: PASS, including snapshot encode/decode across a custom low activation height.

- [ ] **Step 7: Commit**

```powershell
git add core/pow.go core/chain.go core/block.go core/asert_test.go core/core_test.go core/store_test.go
git commit -m "Activate per-block ASERT at height 12096"
```

### Task 3: Explorer and Discord network status

**Files:**
- Modify: `explorer/explorer.go`
- Modify: `explorer/explorer_test.go`
- Modify: `tools/discord/stats-bot.mjs`
- Modify: `tools/discord/stats-bot-cli.test.mjs`

**Interfaces:**
- Consumes: canonical `Params` ASERT fields and `Chain.NextBitsForTip()`.
- Produces: `/api/status` fields `difficulty_algorithm`, `asert_activation_height`, `blocks_to_asert`, `asert_half_life_seconds`, and human network-status text.

- [ ] **Step 1: Write failing status contracts**

Pre-activation status must report `legacy-2016`, activation `12096`, and a non-negative countdown. Post-activation status must report `asert`, half-life `7200`, no future legacy retarget, and the difficulty of `NextBitsForTip`. Discord `/network` must say `ASERT in N blocks` before activation and `ASERT active` after it.

- [ ] **Step 2: Run the focused contracts and confirm red**

Run: `go test ./explorer -count=1; node --test tools/discord/stats-bot-cli.test.mjs`

Expected: FAIL on the missing ASERT fields/text.

- [ ] **Step 3: Implement status projection and human copy**

Add the stable JSON identifiers above. Keep the legacy fields until activation for compatibility, then emit zero countdowns and label ASERT explicitly so no UI displays `retarget in 0 blocks`.

- [ ] **Step 4: Run focused contracts**

Run: `go test ./explorer -count=1; node --test tools/discord/stats-bot-cli.test.mjs`

Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add explorer/explorer.go explorer/explorer_test.go tools/discord/stats-bot.mjs tools/discord/stats-bot-cli.test.mjs
git commit -m "Expose ASERT activation in network status"
```

### Task 4: Mandatory v0.1.34 release surfaces

**Files:**
- Modify: `cmd/btc09/main.go`
- Modify: `cmd/btc09/main_wallet.go`
- Modify: `walletapp/package.json`
- Modify: `walletapp/package-lock.json`
- Modify: `walletapp/src-tauri/tauri.conf.json`
- Modify: `walletapp/src-tauri/tauri.store.conf.json`
- Modify: `walletapp/src-tauri/Cargo.toml`
- Modify: `walletapp/src-tauri/Cargo.lock`
- Modify: `appveyor.yml`
- Modify: `WHITEPAPER.md`
- Modify: `ANNOUNCE.md`
- Modify: `docs/index.html`
- Modify: `docs/EXCHANGE-LISTING.md`
- Modify: `tools/release/test_direct_distribution.py`

**Interfaces:**
- Consumes: activation parameters and existing release scripts.
- Produces: consistent `0.1.34` metadata and short mandatory-upgrade copy.

- [ ] **Step 1: Add failing release contracts**

Require every version surface to contain `0.1.34`, the website to contain `Height 12,096` and `per-block ASERT`, and historical docs to stop claiming that difficulty always retargets every 2,016 blocks.

- [ ] **Step 2: Run release contracts and confirm red**

Run: `python -m unittest tools.release.test_direct_distribution`

Expected: FAIL on old `0.1.33` metadata and legacy-only wording.

- [ ] **Step 3: Update metadata and concise public copy**

State: `Upgrade before height 12,096. Difficulty switches from the old 2,016-block window to per-block ASERT at that height. Wallets remain compatible; node and solo-miner software must update.` Do not promise price, exchange listing, or attack immunity.

- [ ] **Step 4: Run release and content contracts**

Run: `python -m unittest tools.release.test_direct_distribution; go test ./... -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add cmd walletapp appveyor.yml WHITEPAPER.md ANNOUNCE.md docs tools/release/test_direct_distribution.py
git commit -m "Prepare mandatory v0.1.34 network upgrade"
```

### Task 5: Full verification, merge, build, and publish

**Files:**
- Inspect: all changed files and produced artifacts.
- Modify only failures caused by this upgrade.

**Interfaces:**
- Consumes: Tasks 1-4.
- Produces: reviewed master commit, reproducible artifacts, GitHub release, and checksums.

- [ ] **Step 1: Run all local gates**

```powershell
gofmt -w core/params.go core/pow.go core/chain.go core/block.go core/asert_test.go core/core_test.go core/store_test.go explorer/explorer.go explorer/explorer_test.go
go test ./... -count=1
go vet ./...
node --test tools/discord/*.test.mjs
python -m unittest tools.release.test_direct_distribution
git diff --check
```

Expected: every command exits zero.

- [ ] **Step 2: Review consensus diff and activation model**

Confirm no code path applies ASERT below 12,096, no float enters target calculation, mainnet fields match the global constraints, stored block 12,095 replays under legacy rules, and both canonical and side-branch target selection use the correct anchor.

- [ ] **Step 3: Push, open PR, and merge only the reviewed commit**

```powershell
git push -u origin network/asert-daa
gh pr create --repo krutftw/bitcoin09 --base master --head network/asert-daa --title "Replace legacy retarget with per-block ASERT" --body-file docs/plans/2026-07-17-asert-difficulty-upgrade-design.md
```

Read back the PR diff and checks, then squash merge.

- [ ] **Step 4: Build release artifacts from the exact tag**

Tag `v0.1.34`, run the repository's cross-platform release scripts, and let public AppVeyor produce the Windows trusted-build input. Inspect archive members, version metadata, SHA256 values, and executable signatures. Publish only artifacts that passed their platform gates; leave Windows unpublished until SignPath signs it.

- [ ] **Step 5: Publish and read back**

Create GitHub release `v0.1.34` with checksums and mandatory-upgrade notes. Read the release back through `gh release view v0.1.34` and download every asset once to verify its published checksum.

### Task 6: Official-node rollout and activation watch

**Files:**
- Modify: deployed binaries and static site files only after local verification.
- Inspect: systemd units, node logs, explorer API, pool work, Discord bot logs.

**Interfaces:**
- Consumes: exact verified v0.1.34 artifacts.
- Produces: upgraded public infrastructure and live activation evidence.

- [ ] **Step 1: Stage and atomically upgrade every official service**

Back up only replaced binaries, stop one service at a time, install the verified binary, restart it, and verify its version, tip, peers, and logs before proceeding to the next seed. Upgrade the explorer, pool coordinator, wallet gateway, and Discord stats service before the activation height.

- [ ] **Step 2: Verify independent agreement**

For every public seed, compare tip hash and height. Check `https://explorer.btc09.org/api/status`, pool work bits, and Discord live-stat names. Any disagreement stops the rollout and keeps the next service untouched.

- [ ] **Step 3: Publish the website and Discord notice**

Use short copy: `Mandatory 0.1.34 update: node and solo-miner operators need to upgrade before block 12,096. The network moves to per-block difficulty there. Pool miners can keep mining normally.` Link only the official release and upgrade page.

- [ ] **Step 4: Watch activation**

At heights 12,095 through 12,110, record each block's time, bits, computed difficulty, source address, and tip hash from more than one node. Confirm height 12,096 did not take the projected legacy 4-times jump, all official nodes agree, the pool issued matching work, and old-rule blocks are rejected.

- [ ] **Step 5: Close the incident only with live evidence**

After at least 24 post-activation blocks, publish a concise result with actual average block time and difficulty movement. Keep monitoring until at least 100 ASERT blocks have confirmed without a canonical split.
