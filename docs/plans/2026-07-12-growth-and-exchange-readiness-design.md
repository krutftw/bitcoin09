# BTC09 Growth and Exchange Readiness Design

**Date:** 2026-07-12  
**Status:** Approved direction  
**Owner:** BTC09 maintainers

## Goal

The next milestone is not a marketing campaign. It is to make BTC09 easy to
evaluate, integrate, operate, and trust. The practical target is a credible
SafeTrade listing request backed by working software and public evidence, then
reuse the same package for other suitable exchanges.

Success means an exchange engineer can download a checksummed release, sync a
node, create deposit addresses, detect confirmed deposits at an exact chain
tip, prepare and inspect withdrawals, broadcast transactions, and understand
reorg behaviour without reverse-engineering the source. It also means a miner
can see the network's estimated hashrate and recent block distribution without
depending on a third-party pool dashboard.

The project will stay honest about its size. There will be no paid volume,
wash trading, price promises, fake partnerships, or claims that a listing is
confirmed before an exchange says so. Growth copy should be direct and human,
but it should not undersell the working network with repeated statements that
the coin is "worth nothing." The useful message is simpler: fair launch, no
premine, no guarantees, working chain.

## Options considered

### 1. Submit everywhere immediately

This is fast, but weak. The current listing document names several exchanges
without recording whether their present listing routes are still active. It
also says the node is ready for deposits and withdrawals without documenting
the machine interfaces that already exist. A reviewer who cannot quickly find
those details is likely to stop there.

### 2. Build a BTC09 exchange

This would create custody, security, liquidity, compliance, and operational
work far beyond the coin itself. It would also compete with the actual goal of
getting independent venues to integrate BTC09. The Discord escrow bot remains
useful for small community trades, but it is not the growth centrepiece.

### 3. Build an exchange-readiness package, then submit selectively

This is the chosen approach. BTC09 already has strict JSON machine commands for
wallet creation, tip-bound snapshots, transaction preparation, independent
inspection, and broadcast. The explorer already has versioned endpoints for
tips, blocks, transactions, and address outputs. The missing part is a clear
operator contract, a reproducible smoke test, public network evidence, and a
listing request that points to all of it.

## Deliverables

### Exchange integration

Add a versioned exchange integration guide covering:

- supported release and checksums;
- Linux node setup and safe localhost-only explorer binding;
- exact `/api/v1` response contracts for tip, block, transaction, and address
  output lookup;
- machine wallet commands and their JSON success/failure envelopes;
- unique deposit address creation;
- confirmation and coinbase-maturity policy;
- tip-pinned deposit scans and what to do on a tip mismatch;
- withdrawal preparation, transaction inspection, reserved outpoints, and
  broadcast;
- backups, wallet-file permissions, hot-wallet limits, and recovery;
- a recommended initial confirmation count that an exchange may adjust after
  monitoring the network.

Add a smoke-test tool that checks a synced node and wallet without exposing
private keys or moving funds. The public listing spec will link to this guide
instead of claiming readiness without evidence.

### Network transparency

Extend the explorer status data and website with an estimated network hashrate
derived from observed block time and current proof-of-work. Label it as an
estimate. Add recent block-producer concentration for fixed public windows,
using coinbase payout addresses as the grouping key. This is not identity and
must be described as address concentration, since one miner may use several
addresses and several miners may share a pool address.

Add a solo-mining probability calculator to the website. It should explain
expected time and variance from a miner's entered hashrate without implying a
guarantee. This answers the valid Discord complaint directly: a pool smooths
variance, but it does not increase a miner's expected share before fees.

No consensus rule will be changed in this milestone. Pool dominance is a
distribution problem, not something to patch with arbitrary block rejection.
The longer-term answer is multiple independent pools and open pool-mining
software, not an official monopoly pool.

### Reliability and operations

Keep the current DigitalOcean firewall and localhost-only explorer backend.
The next infrastructure purchase should split public P2P from the website,
explorer, and OTC service so a flood against port 9009 cannot take every public
surface offline together. HTTP services can then sit behind Cloudflare while
the public P2P node remains independently reachable.

That split requires a new recurring-cost DigitalOcean Droplet, so creation is
outside this code milestone and must be confirmed at purchase time. It should
use a mainstream region close to existing users and exchanges, not Australia.

## Rollout order

1. Publish the integration guide, smoke test, corrected listing spec, and
   matching release artifacts.
2. Publish network hashrate, concentration, and solo probability data on the
   explorer and main website.
3. Run the full Go and Python suites, build all release targets, inspect the
   artifacts, and verify the public endpoints after deployment.
4. Send one concise SafeTrade listing request with links to the integration
   guide, source, release, explorer, community, and fair-launch evidence.
5. Reuse the verified package for other active exchanges only after checking
   their current submission route and requirements.

## Measures of success

The milestone is complete when the integration flow is reproducible on a clean
Linux environment, the public metrics match chain-derived calculations, the
website and repository agree on current facts, and the deployed services pass
read-back checks. The listing request is a separate external action: submission
is recorded only after the support system confirms receipt, and a listing is
announced only after the exchange confirms it.

Growth after that will be judged by independent public nodes, distinct recent
mining addresses, release downloads, real trades, returning Discord members,
and exchange progress. Follower counts and noisy posts are secondary.
