# Bitcoin 09 v0.1.21

This release adds an open mining path that does not require a closed-source
miner or a local full node.

Changes:

- `btc09 mine-pool` is an open-source remote-solo miner built into the official
  Go client.
- `btc09 node -solo-api` lets any synced node run an independent coordinator.
- The coordinator creates canonical templates, pays the miner's chosen 09C
  address, and accepts only a nonce for a short-lived server-owned job.
- The mining API has strict small JSON bodies, finite timeouts, safe errors,
  bounded jobs, duplicate and stale checks, and per-source rate limits.
- Discord `/stats` is now routed correctly and official chain stats no longer
  depend on a third-party pool API being online.
- A hardened seed-only systemd unit and TLS reverse-proxy example are included
  for independent operators.
- GitHub Actions now runs the Go race detector, `go vet`, Discord tests, and the
  website contract on every push and pull request.

This is remote solo mining, so it does not smooth payout variance. PPLNS is not
live in the official software. The planned non-custodial PPLNS phase will not be
enabled until its payout accounting, persistence, restart, and reorg behavior
have a separate security review.

There are no consensus changes. Existing v0.1.20 nodes remain compatible.
Check `SHA256SUMS.txt` before running a downloaded binary.
