# PPLNS share difficulty floor

**Date:** 2026-08-06
**Status:** Investigated, not implemented. Needs a protocol version.

## Problem

A CPU pointed at the official pool waits more than an hour to prove it is
working at all. That is almost certainly why the pool has two miners and has
never found a block, and no amount of promoting the pool fixes it.

Measured on mainnet on 2026-08-06:

```
share_target        0000ffff00000000...   = 65,537 expected hashes
pool hashrate       68.1 H/s
observed interval   1,278 s between shares (~21 minutes)
network difficulty  25.85
```

65,537 expected hashes at a plausible single-machine CPU rate of 10 to 20 H/s
is 55 to 110 minutes for one share. A new miner sees nothing happen, concludes
the pool is broken, and leaves.

For comparison, LuckyPool runs Exfer, a chain with the same Argon2id 64 MiB
shape, at shares of 500 to 20,000 hashes with per-connection vardiff. Our floor
is 3.3x harder than their default and 131x harder than their minimum.

## Root cause

`PPLNSCoordinator.Issue` computes:

```go
shareTarget := networkTarget * ShareTargetMultiplier   // default 64
if shareTarget > params.MaxTarget() { shareTarget = params.MaxTarget() }
```

A larger target is easier. `MaxTarget` on mainnet is `0000ffff...`, which is
65,537 expected hashes. While network difficulty is below the multiplier, the
product always exceeds `MaxTarget` and the cap swallows it, so the multiplier
has no effect whatsoever and every share costs exactly `MaxTarget`. At the
current difficulty of ~26 the intended "64 times easier than a block" is
clamped to "as hard as the easiest possible block".

The floor is not a consensus requirement. A share is pool payout accounting,
not a block. Blocks are still checked against `networkTarget` separately in
`Submit`, so shares are free to be easier without touching consensus.

## Why this is not a one-line change

Two independent implementations enforce the same bound, so easing the server
alone produces jobs that existing miners refuse:

- `pool/pplns_work.go` rejects `share_target > params.MaxTarget()` when a client
  validates work it was handed.
- The community CUDA miner enforces the identical rule in
  `src/pplns_verify.cpp` (`cmp_uint256(share_target, max_target) > 0` fails),
  and again in its payout-proof path. That miner is currently the only
  open-source accelerated miner that speaks our pool protocol, so breaking it
  costs more than the fix gains.

Easing the share target is therefore a **protocol change** and needs a version,
not an edited constant. Existing clients must keep receiving jobs they accept.

A third enforcement point, found on the first implementation attempt, is the
one that actually decides the design. `validatePPLNSState` in `pool/pplns.go`
rejects any **stored** share whose target is easier than `MaxTarget`, and the
share window travels inside every issued job, where every client re-validates
it, including the community CUDA miner's independent port of the same rules.
That means a per-job opt-in is not enough: the moment one eased share enters
the window, every subsequent job fails validation for every legacy client,
whether or not that client opted in. The window is a shared, cross-verified
ledger, and its rules are effectively part of the wire protocol.

The fix therefore requires bumping the PPLNS state schema version, updating
all three validators together (coordinator, official client, community
miners), and migrating the crash-durable window file. It is a coordinated
release, not a server-side deploy.

A second trap: on regtest `MaxTarget` is already about 2^255, roughly one hash
in two. Multiplying it and clamping naively to 2^256-1 makes every possible
hash a valid share, and any test that searches for a *rejected* share then
never terminates. An attempt at this hung the pool suite until timeout. Any
implementation must leave regtest's effective share target unchanged.

## Constraints any fix must respect

- **Submit rate limit.** nginx allows `120r/m` (2 per second) per address on
  `/api/v2/pool/submit`, and `12r/m` on `/api/v2/pool/work`. Shares of ~4,096
  hashes keep a 3.6 kH/s GPU near 0.9 submissions per second, which fits. Much
  easier than that and a single fast card rate-limits itself.
- **Window rotation.** The window holds 256 shares. Easier shares rotate it
  faster, which shortens the PPLNS lookback. That is normal PPLNS behaviour but
  it changes payout dynamics and deserves to be stated publicly when it changes.
- **Durability cost.** Each accepted share rewrites the crash-durable window
  file. More shares means more fsyncs.

## What does not need fixing

Payout fairness is already correct for mixed difficulties. `pplnsPayouts`
weights by `core.WorkFromTarget(shareTarget)`, not by share count, so a miner
submitting sixteen shares at sixteen times easier difficulty earns exactly what
one harder share would have earned. Per-connection vardiff can therefore be
added later without reworking payouts.

## Options

1. **Versioned easier shares.** Add a v3 work route, or an explicit opt-in
   parameter on v2, that may return a share target easier than `MaxTarget`,
   bounded to roughly 16x (about 4,096 expected hashes on mainnet). v2 keeps
   today's semantics so existing miners are untouched. Smallest change that
   fixes the first-run experience.
2. **Per-connection vardiff.** What real pools do, and what the Exfer
   comparable runs. Targets a share every N seconds per miner regardless of
   hardware, which is the only approach that serves a 15 H/s CPU and a 3,600
   H/s GPU from one pool. Larger change: per-session difficulty state, a
   retarget loop, and the same protocol versioning as option 1.
3. **Do nothing and say so.** Defensible only while the pool is not being
   promoted. It is now linked from the mining guide, so this is the weakest
   option.

Recommended: option 1 to unblock, with option 2 as the real answer once more
than a handful of miners are connected.

## Coordination

Whoever implements this should tell the author of the community CUDA and Apple
Silicon miners before shipping, so their clients accept the new targets in the
same window. They have been responsive and their miners already support the
official pool.
