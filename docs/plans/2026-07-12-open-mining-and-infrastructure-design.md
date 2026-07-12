# Open Mining and Infrastructure Separation Design

**Status:** approved for implementation by project owner delegation

**Date:** 2026-07-12

## Objectives

Bitcoin 09 needs a second practical mining path that is fully open source, does not depend on a closed third-party miner, and does not replace one concentration problem with an official monopoly pool. The public P2P seed also needs a different failure domain from the website, explorer, Discord OTC service, and public trade feed.

The work must not change consensus. Existing v0.1.20 nodes must continue to validate every block produced by the new mining path.

## Options considered

### Run a single official custodial pool

This is the fastest familiar user experience, but it puts the project in control of miner balances, payout accounting, and a central mining endpoint. It adds financial custody and an attractive attack target. It would improve variance for small miners while making the network and the project more dependent on one operator. This option is rejected.

### Adapt a generic Bitcoin Stratum server

Most existing Stratum servers assume Bitcoin-family JSON-RPC, SHA-256d or a supported altcoin algorithm, and Bitcoin transaction and block serialization. Bitcoin 09 has an 88-byte header, Argon2id proof of work, Ed25519 transactions, and no `getblocktemplate` RPC. A compatibility layer would be larger and harder to audit than a small native protocol. This option may become useful later as a miner-facing adapter, but it is not the first implementation.

### Build a native, self-hostable coordinator and client

This is the selected approach. The coordinator and miner live in the official Go repository and use the same canonical block, address, target, and Argon2id code as the node. Anyone can run the coordinator. The project can operate a bootstrap endpoint, but the documentation and protocol make independent community operation the normal model.

## Mining architecture

Phase one is non-custodial remote solo mining. A coordinator syncs as a normal 09C node, builds canonical templates, and exposes a small versioned HTTP API on a separately configured address. A miner requests work using a valid 09C payout address and worker label. The coordinator returns an opaque job identifier, the exact 88-byte header with a zero nonce, the network target, the share target, and an expiry. The miner changes only the nonce and submits the job identifier plus nonce. The coordinator reconstructs the header from its stored job, calculates Argon2id itself, rejects duplicate, stale, malformed, or low-difficulty submissions, and broadcasts a valid network block through normal P2P.

The first protocol deliberately does not accept arbitrary transaction sets, coinbase bytes, targets, or headers from clients. That keeps consensus construction on the coordinator and limits an untrusted client to choosing a payout address and searching nonces. Jobs are short-lived, bound to a chain tip, bounded in memory, and issued under per-IP limits. Share difficulty is high enough that verification cannot be used as a cheap 64 MiB Argon2 denial-of-service amplifier. The HTTP service is intended to sit behind TLS and connection limits, with its P2P node on a separate process or host.

Phase two adds non-custodial PPLNS. Accepted shares form a bounded, durable window. At each template refresh the coordinator constructs a multi-output coinbase paying participants directly in proportion to valid work. The pool never owns a payout wallet and never holds miner funds. Output count, address count, rounding, share weight, window persistence, reorg handling, and restart recovery require a separate security review before mainnet activation. Phase one lays down the protocol and independent-operator path without pretending those accounting rules are already finished.

## Public statistics and Discord

Official statistics must be derived from the chain and official node API. Third-party pool data may be shown as an explicitly labeled optional section, but its failure must never break `/stats` or the pinned network status message. The current bot is changed to make the explorer status response authoritative for height, peers, estimated network hashrate, difficulty, retarget progress, and payout-address concentration.

The human-facing message should say what the chain can prove. A payout address is not necessarily one person, and pool-reported worker counts are not network miner counts. If the third-party endpoint is unavailable, the bot should omit that section and still publish a useful update. This avoids giving a closed pool operational control over the project's Discord status surface.

## DigitalOcean separation

The existing Singapore Droplet remains the web origin and runs Nginx, the explorer API, OTC bot, and public OTC feed. After cutover, its Cloud Firewall no longer permits TCP 9009. Cloudflare continues to protect the HTTP hostnames and conceal normal web origin traffic, but it is not represented as protection for raw P2P.

A new small Singapore DigitalOcean Droplet runs only the 09C seed service. Initial sizing is 1 GiB RAM because the current node process and 3.2 MiB chain fit comfortably, while a swap file and a 1 GiB systemd memory limit protect the host. Its Cloud Firewall permits SSH from the administrator's current management source when practical and TCP 9009 globally. It does not expose ports 80, 443, or 8009. The seed gets its own hostname and IP.

Cutover is staged: provision and harden the new host, copy a verified chain snapshot, start v0.1.20, confirm height and peers, update DNS, publish a release with the new built-in seed, observe both seeds, then remove 9009 from the web Droplet. The old seed remains available during propagation so existing binaries do not lose their only official bootstrap path. A later release can remove the old IP once adoption is sufficient. This separation limits the blast radius of future P2P floods without claiming that a firewall can absorb a multi-million-packet volumetric attack on an allowed port.

## Release gates

- Protocol tests prove canonical job encoding, nonce-only submission, stale-job rejection, duplicate rejection, share-target validation, and network-block acceptance.
- Race tests pass for job issuance and submission.
- Existing `go test ./...` and OTC tests remain green.
- The open miner and coordinator work end to end on regtest before any mainnet endpoint exists.
- A mainnet coordinator is not advertised as pooled mining until non-custodial PPLNS accounting and restart/reorg behavior pass a dedicated review.
- The new seed is synced and reachable before DNS or built-in seed changes ship.
- No DigitalOcean resource is created until its exact recurring price is confirmed at the action point.
