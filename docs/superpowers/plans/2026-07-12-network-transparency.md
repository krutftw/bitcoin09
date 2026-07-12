# Network Transparency Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish chain-derived network hashrate, recent payout-address concentration, and an honest solo-mining probability calculator.

**Architecture:** Compute mining estimates inside the existing explorer from canonical blocks only. Add the values to the legacy status JSON and explorer home page, then have the static website render the official chain data independently of the third-party pool API. Keep the estimator descriptive and make its limits explicit.

**Tech Stack:** Go 1.25, `math/big`, existing explorer HTTP server and templates, static HTML/CSS/JavaScript, Go tests, Python standard-library site assertions.

## Global Constraints

- Do not change consensus, mining validity, retarget rules, or block selection.
- Group blocks by coinbase payout address and call the result address concentration, not miner identity.
- Estimated hashrate must be labelled as an estimate and include its observation window.
- The calculator must describe expected value and variance, never guaranteed block time.
- Third-party pool data stays clearly separated from official chain-derived data.
- Public copy must be plain English and must not blame or endorse a specific pool.

---

### Task 1: Chain-derived mining statistics

**Files:**
- Modify: `explorer/explorer.go`
- Modify: `explorer/explorer_test.go`

**Interfaces:**
- Consumes: `core.WorkFromTarget(core.CompactToTarget(bits))`, canonical `Chain.BlockAt(height)`, and `retargetData`.
- Produces: `miningStatsAt(tip int64, retarget retargetData) miningStats`, JSON fields in `/api/status`, and explorer home-page fields.

- [ ] **Step 1: Write failing estimator tests**

```go
func TestMiningStatsEstimateAndConcentration(t *testing.T) {
    chain := testChainWithPayouts(t, 120, []payoutRun{{Address: addressA, Blocks: 90}, {Address: addressB, Blocks: 30}})
    server := mustExplorer(t, chain)
    stats := server.miningStatsAt(120, retargetData{
        EpochElapsedBlocks: 120,
        EpochElapsedSeconds: 600,
        EpochAverageBlockSeconds: 5,
    })
    if stats.EstimatedNetworkHashrateHPS <= 0 || stats.HashrateObservationBlocks != 120 || stats.HashrateObservationSeconds != 600 {
        t.Fatalf("unexpected estimator: %#v", stats)
    }
    if len(stats.Windows) != 2 || stats.Windows[0].RequestedBlocks != 100 || stats.Windows[0].ObservedBlocks != 100 {
        t.Fatalf("unexpected windows: %#v", stats.Windows)
    }
    if stats.Windows[0].TopPayoutAddress != addressA || stats.Windows[0].TopSharePercent != 70 {
        t.Fatalf("unexpected 100-block concentration: %#v", stats.Windows[0])
    }
}

func TestMiningStatsEmptyEpochDoesNotInventHashrate(t *testing.T) {
    server := mustExplorer(t, testChain(t))
    stats := server.miningStatsAt(0, retargetData{})
    if stats.EstimatedNetworkHashrateHPS != 0 || stats.HashrateObservationBlocks != 0 || len(stats.Windows) != 0 {
        t.Fatalf("genesis stats must be empty: %#v", stats)
    }
}
```

- [ ] **Step 2: Run the focused tests**

Run: `go test ./explorer -run 'TestMiningStats' -count=1`

Expected: FAIL because `miningStatsAt` and the mining-stat types do not exist.

- [ ] **Step 3: Implement the estimator and fixed windows**

```go
type miningWindow struct {
    RequestedBlocks       int64   `json:"requested_blocks"`
    ObservedBlocks        int64   `json:"observed_blocks"`
    DistinctPayoutAddresses int    `json:"distinct_payout_addresses"`
    TopPayoutAddress      string  `json:"top_payout_address"`
    TopPayoutBlocks       int64   `json:"top_payout_blocks"`
    TopSharePercent       float64 `json:"top_share_percent"`
}

type miningStats struct {
    EstimatedNetworkHashrateHPS float64        `json:"estimated_network_hashrate_hps"`
    HashrateObservationBlocks   int64          `json:"hashrate_observation_blocks"`
    HashrateObservationSeconds  int64          `json:"hashrate_observation_seconds"`
    Windows                     []miningWindow `json:"payout_address_windows"`
}

func expectedHashes(bits uint32) float64 {
    work := core.WorkFromTarget(core.CompactToTarget(bits))
    value, _ := new(big.Float).SetInt(work).Float64()
    return value
}

func (s *Server) miningStatsAt(tip int64, retarget retargetData) miningStats {
    var out miningStats
    if tip <= 0 || retarget.EpochElapsedBlocks <= 0 || retarget.EpochElapsedSeconds <= 0 {
        return out
    }
    block := s.chain.BlockAt(tip)
    if block == nil {
        return out
    }
    out.EstimatedNetworkHashrateHPS = expectedHashes(block.Header.Bits) * float64(retarget.EpochElapsedBlocks) / float64(retarget.EpochElapsedSeconds)
    out.HashrateObservationBlocks = retarget.EpochElapsedBlocks
    out.HashrateObservationSeconds = retarget.EpochElapsedSeconds
    for _, requested := range []int64{100, 500} {
        observed := requested
        if observed > tip {
            observed = tip
        }
        counts := make(map[string]int64)
        for height := tip - observed + 1; height <= tip; height++ {
            row, ok := s.row(height)
            if ok && row.Miner != "unspendable" {
                counts[row.Miner]++
            }
        }
        if observed == 0 || len(counts) == 0 {
            continue
        }
        window := miningWindow{RequestedBlocks: requested, ObservedBlocks: observed, DistinctPayoutAddresses: len(counts)}
        for address, blocks := range counts {
            if blocks > window.TopPayoutBlocks || (blocks == window.TopPayoutBlocks && address < window.TopPayoutAddress) {
                window.TopPayoutAddress, window.TopPayoutBlocks = address, blocks
            }
        }
        window.TopSharePercent = float64(window.TopPayoutBlocks) * 100 / float64(observed)
        out.Windows = append(out.Windows, window)
    }
    return out
}
```

Use the deterministic address tie-breaker shown above. If malformed historical coinbase data causes fewer counted addresses than observed blocks, the percentage denominator remains the canonical observed block count and the missing rows are not assigned to an invented address.

- [ ] **Step 4: Add API and explorer-page fields**

Add `estimated_network_hashrate_hps`, `hashrate_observation_blocks`, `hashrate_observation_seconds`, and `payout_address_windows` to `/api/status`. Add `NetworkHashrate`, `HashrateWindow`, and `PayoutConcentration` display fields to `homeData`, and render text such as `estimated network 16.3 KH/s over 699 blocks / 180,364s` and `top payout address: 93 of last 100 blocks`.

- [ ] **Step 5: Test status JSON and template text**

Extend the existing `/api/status` test to decode the fields and assert finite nonnegative numbers, fixed requested windows, and percentages between 0 and 100. Extend the home-page test to assert `estimated network` and `top payout address` appear only when sufficient history exists.

Run: `go test ./explorer -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add explorer/explorer.go explorer/explorer_test.go
git commit -m "Expose chain-derived mining statistics"
```

### Task 2: Website network panel and solo calculator

**Files:**
- Modify: `docs/index.html`
- Create: `tools/site/test_index_contract.py`

**Interfaces:**
- Consumes: `/api/status` fields produced by Task 1.
- Produces: official network hashrate and concentration cards plus `soloEstimate(minerHashrate, networkHashrate, blockSeconds) -> object` in the static page.

- [ ] **Step 1: Write failing static contract tests**

```python
class IndexContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.html = pathlib.Path("docs/index.html").read_text(encoding="utf-8")

    def test_official_mining_metrics_are_rendered(self):
        for token in ("stat-network-hashrate", "stat-top-share-100", "estimated_network_hashrate_hps", "payout_address_windows"):
            self.assertIn(token, self.html)

    def test_solo_calculator_is_explicit_about_variance(self):
        for token in ("solo-hashrate", "solo-estimate", "Expected time, not a guarantee", "function soloEstimate"):
            self.assertIn(token, self.html)
```

- [ ] **Step 2: Run the contract tests**

Run: `python -m unittest tools.site.test_index_contract -v`

Expected: FAIL because the new elements and calculator do not exist.

- [ ] **Step 3: Add official metric cards**

Add these cards inside the existing `Network Now` grid:

```html
<div class="stat"><b id="stat-network-hashrate">-</b><span>estimated network hashrate</span></div>
<div class="stat"><b id="stat-top-share-100">-</b><span>top payout address, last 100 blocks</span></div>
<div class="stat"><b id="stat-distinct-100">-</b><span>payout addresses, last 100 blocks</span></div>
```

In `refreshStats()`, use the window whose `requested_blocks === 100`, call the existing `hashRate()` formatter, and render the top share with one decimal place. If the API lacks the new fields, render `-` without falling back to third-party pool data.

- [ ] **Step 4: Add the solo probability calculator**

```javascript
function soloEstimate(minerHashrate, networkHashrate, blockSeconds) {
  const miner = Number(minerHashrate);
  const network = Number(networkHashrate);
  const seconds = Number(blockSeconds);
  if (!(miner > 0) || !(network > 0) || !(seconds > 0)) return null;
  const share = Math.min(miner / network, 1);
  const expectedSeconds = seconds / share;
  return {
    share,
    expectedSeconds,
    chanceInOneHour: 1 - Math.exp(-3600 / expectedSeconds),
    chanceInOneDay: 1 - Math.exp(-86400 / expectedSeconds),
  };
}
```

Add a numeric H/s input with id `solo-hashrate`, output id `solo-estimate`, and copy that says: `Expected time, not a guarantee. Solo mining has high variance. A pool smooths payouts but does not increase your expected share before fees.` Render network share, expected block interval, one-hour chance, and one-day chance. Use the current official estimated network hashrate and observed average block time, falling back to target block seconds only when the observed average is unavailable.

- [ ] **Step 5: Keep the pool section separate**

Change the official stats note to mention the estimator observation block count. Keep the community pool heading and disclaimer unchanged in meaning. Do not use the pool API as the source for official network hashrate or concentration.

- [ ] **Step 6: Run focused and full tests**

Run:

```bash
python -m unittest tools.site.test_index_contract -v
go test ./...
python -m unittest discover -s bot/tests -v
git diff --check
```

Expected: all focused tests PASS, Go PASS, Python PASS with documented skips only, no whitespace errors.

- [ ] **Step 7: Commit**

```bash
git add docs/index.html tools/site/test_index_contract.py
git commit -m "Add mining transparency and solo estimates"
```

### Task 3: Deploy and public read-back

**Files:**
- Verify: production explorer and website.

**Interfaces:**
- Consumes: Tasks 1-2 and the existing deployment runbook.
- Produces: public chain-derived metrics that agree between JSON, explorer HTML, and the website.

- [ ] **Step 1: Deploy the node/explorer change**

Build the exact committed revision on the DigitalOcean host, replace the seed binary through the existing atomic deployment path, restart `btc09-seed`, and confirm the service stays active through at least two heartbeat intervals.

- [ ] **Step 2: Deploy the static website**

Publish `docs/index.html` through the existing site deployment path and purge only the affected cached page if required.

- [ ] **Step 3: Compare public values**

Read `https://explorer.btc09.org/api/status`, verify a finite positive `estimated_network_hashrate_hps`, verify the 100/500 block windows, and calculate `expectedHashes(current bits) * observationBlocks / observationSeconds` independently. The displayed website value must match after unit formatting and rounding.

- [ ] **Step 4: Browser-check desktop and mobile layouts**

Check the main page at a normal desktop viewport and a narrow mobile viewport. Confirm the new cards do not overflow, the calculator works for `100 H/s`, and the third-party pool disclaimer remains visible.

- [ ] **Step 5: Push and read back GitHub**

Push the verified commits, read back the remote commit SHA and public files, then post one short Discord update only after the website is live. The update should say what changed and link the explorer; it should not mention AI, call the pool malicious, or promise earnings.

