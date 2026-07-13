# BTC09 Official Miner Design

**Date:** 2026-07-13  
**Status:** Approved by owner delegation

## Decision

Ship an official open-source miner inside the BTC09 desktop wallet. The first
release mines against an official remote-solo coordinator, so it works in Fast
wallet mode without downloading the chain. It pays any found block directly to
the user's wallet address. It does not hold coins, calculate pool shares, or
depend on NTMminer.

This is the current project priority. Live Discord messages show newcomers
passing around NTMminer binaries through MediaFire after its GitHub disappeared,
asking whether the binary must be unzipped, reporting crashes, and questioning
whether it is a scam. BTC09 already includes a safe open CLI miner and an open
remote-solo protocol, but both are hidden and provide poor feedback. The fix is
to make the official path obvious and usable.

## Product boundary

The first screen calls the mode **Open solo**, never "pool." A short sentence
explains the tradeoff: a found block pays the selected address directly, but
there are no partial-share payouts and results can be rare. A link explains that
third-party pooled mining is separate software and is not endorsed or bundled.

The miner screen provides:

- automatic selection of the wallet's current receive address;
- a CPU thread slider, logical CPU count, and a leave-one-thread-free default;
- an optional plain worker label;
- Start and Stop controls;
- current and session-average H/s, total hashes, elapsed time, jobs received,
  reconnect count, and blocks accepted;
- current state: stopped, connecting, mining, retrying, stopping, or error;
- the last useful error in human language;
- a clear note that private keys and the wallet file never go to the coordinator.

Temperature, profitability, GPU controls, automatic startup, background mining,
and pooled balances are excluded. The app does not imply that more threads
always produce more H/s; it recommends testing one step below all logical CPUs.

## Architecture

`pool.MineWorkWithProgress` extends the existing nonce search with a low-rate
progress callback based on the existing atomic hash counter. It never inserts
work into the hot hash loop beyond the counter already present. `RemoteClient`
adds an event stream for job, progress, accepted block, retry, and fatal states.
Transient transport, 429, and 5xx errors use bounded exponential backoff with
jitter; invalid work, cross-network data, and authentication errors stop.

The desktop package gains an optional `MinerService` interface, leaving existing
wallet-only test services source-compatible. The concrete app service owns one
miner session, a cancel function, and an immutable status snapshot protected by
a mutex. Start rejects duplicate sessions, invalid thread counts, missing
wallets, and unavailable addresses. Stop is idempotent. Closing the app cancels
the session.

The local wallet server exposes authenticated same-origin routes:

- `GET /api/v1/miner/status`
- `POST /api/v1/miner/start` with `{ "workers": N, "worker": "label" }`
- `POST /api/v1/miner/stop` with `{}`

The browser polls status once per second only while the wallet is open. The
coordinator URL is a release default/configuration value, not a field where a
new user can paste an arbitrary server. Advanced miners keep the existing CLI
`mine-pool -pool` override.

## Public coordinator

The existing coordinator already constructs a unique block template containing
the miner's payout address, returns canonical header work, accepts only a nonce,
reconstructs the full block, validates it, and broadcasts it. It is remote solo,
not custodial pooled mining.

The main DigitalOcean node will listen on `127.0.0.1:9010`. Nginx and Cloudflare
publish only `POST /api/v1/work` and `POST /api/v1/submit` on the chosen HTTPS
origin, with body, connection, request, and upstream time limits. No direct VPS
port is opened. Work creation and invalid submissions are rate-limited
separately. The node, explorer, wallet gateway, and coordinator remain isolated
at the routing layer even if they share a binary process.

The client validates network identity, Argon2 parameters, target, header shape,
expiry, response size, content type, and submit result. Only the public payout
address and optional worker label reach the coordinator.

## Compatibility and open protocol

BTC09 will publish the existing JSON protocol as **Open Mining Protocol v1**.
It is intentionally small: request work, vary the 64-bit nonce, and submit a
network-winning nonce. Reference vectors include a canonical header, target,
Argon2 parameters, invalid cases, and regtest integration.

This gives third-party developers a stable target without reverse engineering
NTMminer. It does not claim Stratum compatibility. Stratum V2 is built around
pool channels and share submission; adopting its framing without a real share
accounting system would add complexity without interoperability.

A real open pooled-share service is a separate milestone. It requires share
difficulty, accounting, maturity handling, payout policy, anti-abuse controls,
and public operator terms. It will be designed deliberately after the official
miner removes today's unsafe-download problem.

## Visual direction

The miner lives inside the existing wallet design but is not another oversized
marketing panel. A compact instrument strip shows H/s, hashes, elapsed time,
and blocks. The thread control uses a labelled slider plus exact number. State
changes use the wallet's existing ink, paper, and yellow signal palette. A
single restrained activity line replaces a fake terminal log.

At 390x844 the controls stack in one column and the instrument strip becomes a
two-column grid. At 1280x800 the miner fits without hiding Start/Stop below the
fold. Text remains balanced with the wallet; no giant numbers, neon gradients,
speedometer gauges, flames, GPU art, or casino styling are used.

## Verification and rollout

Tests cover hash progress accuracy, cancellation, no callback hot-loop leak,
retry classification and backoff, event ordering, session concurrency, wallet
address selection, local API authentication, start/stop idempotence, and UI
state contracts. A regtest coordinator proves end-to-end block acceptance.

Release verification includes Go tests, vet, Linux race tests, vulnerability
scan, Windows/Linux/macOS builds, desktop visual inspection, sustained local
mining with changing thread counts between sessions, and public coordinator
tests through Cloudflare. Existing services are checked before and after live
deployment.

Only after live mining succeeds will the website and Discord direct people to
the official miner. Discord copy will state that NTMminer is third-party and
that the official mode is solo, so users can choose with accurate expectations.
