# ASERT difficulty upgrade

**Status:** approved for implementation on 2026-07-17

## Problem

BTC09 still uses Bitcoin's 2,016-block retarget with a four-times adjustment
limit. At mainnet height 11,441, the current window was averaging 106 seconds
per block at difficulty 679.66 and the explorer projected the maximum increase
to 2,718.62 at height 12,096. A large miner can therefore mine the low part of
the window, leave after the discrete increase, and strand the remaining miners
at roughly 30 to 40 minute blocks. The previous two windows also reached the
four-times ceiling, so this is a structural problem rather than a one-off burst.

The upgrade must preserve every historical block, avoid an abrupt difficulty
jump, react in both directions, remain deterministic on every platform, and be
deployable before height 12,096.

## Decision

Mainnet activates integer ASERT at height **12,096**. Block 12,095 is the
anchor and block 12,094 supplies the anchor-parent timestamp. Blocks below the
activation height keep the exact legacy calculation. Starting at activation,
every block derives its required target from the anchor target, elapsed time,
height difference, 600-second target spacing, and a **7,200-second half-life**.
The target remains bounded by the existing proof-of-work limit.

The calculation follows the published aserti3 fixed-point polynomial and uses
integer arithmetic only. The half-life is a consensus parameter, not a runtime
setting. A two-hour half-life was selected because BTC09's observed hash input
changes by several times within hours. A deterministic step model using the
live 5.6-times equilibrium gap reaches 7.7-minute expected blocks after 48
blocks and 9.8 minutes after 96. Following a 5.6-times hash withdrawal, the
expected interval falls from an unavoidable first 56-minute block to 25
minutes by block 6, 17 minutes by block 12, 12.5 minutes by block 24, and 10.5
minutes by block 48. The standard two-day half-life would still be near 38
minutes after 48 blocks and is too slow for this network.

## Timestamp rules

A short ASERT half-life makes honest timestamps important. At and after height
12,096, a block timestamp must be strictly greater than the median of the prior
11 blocks and no more than 30 minutes ahead of the validating node's clock.
The existing pre-activation timestamp rules remain unchanged. Thirty minutes
leaves ample room for ordinary clock skew while limiting the maximum target
shift available from future dating to about 19 percent. Median-time-past stops
a single miner from moving time backwards to manipulate the schedule.

## Alternatives considered

- Keeping the legacy retarget and adding only an emergency escape would still
  preserve the profitable low-difficulty window and create discontinuities.
- DigiShield or a short moving window would react quickly, but moving windows
  can oscillate when old fast or slow blocks leave the window. That is the same
  positive-feedback class this upgrade is intended to remove.
- Standard two-day ASERT is well proven on Bitcoin Cash, but simulations against
  BTC09's live hash swing show that it cannot recover quickly enough on a small
  network. ASERT's published half-life parameter exists for this tuning.

## Implementation and verification

`core.Params` carries activation height, half-life, post-activation future
drift, and median-time window. `NextBits` selects the historical or ASERT rule
by height. Both canonical-tip and side-branch validation use the same function;
stored chains replay all pre-activation blocks under the historical rules.

Tests must cover the published Bitcoin Cash ASERT vectors, steady/fast/slow
schedules, proof-of-work limit clamping, exact activation behavior, legacy
history preservation, branch-specific anchors, timestamp boundaries, store
reload across activation, and rejection of old-rule blocks after activation.
The full Go suite, race-sensitive package tests, JavaScript contracts, native
wallet tests, release contract tests, and clean cross-platform builds must pass.

## Rollout

Version 0.1.34 is a mandatory network upgrade. All official seed nodes, the
explorer, pool coordinator, Discord services, and wallet gateway are upgraded
before activation. Release notes, the website, and Discord state the activation
height plainly and tell node and solo-miner operators to upgrade. Pool miners do
not choose block difficulty themselves and only need the normal current miner;
the upgraded coordinator supplies valid work. After activation, the explorer is
checked for ASERT bits, canonical agreement across seeds, peer height, block
cadence, and rejection of a legacy 4-times target.
