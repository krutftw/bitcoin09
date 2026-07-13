# BTC09 Open Mining Protocol v1

Open Mining Protocol v1 is BTC09's small, public remote-solo interface. A miner
asks a synced coordinator for a canonical block header, searches the 64-bit
nonce locally, and submits a nonce only when it meets the network target.

This is not a pooled-share protocol. There are no partial-share balances or
operator-held rewards. A winning block pays the address in the original work
request directly through its coinbase output.

The official mainnet coordinator is `https://mine.btc09.org`. Third-party
coordinators can implement the same two routes.

## Transport

- HTTPS is required outside loopback development.
- Requests use `Content-Type: application/json` and one JSON value only.
- Unknown fields, bodies over 4096 bytes, and non-POST methods are rejected.
- Responses use JSON, `Cache-Control: no-store`, and schema version `1`.
- Clients must apply finite connect, request, and response-size limits.

## Request work

`POST /api/v1/work`

```json
{
  "address": "YOUR_09C_ADDRESS",
  "worker": "home-pc"
}
```

`address` is a valid BTC09 payout address. `worker` is optional and can contain
up to 64 ASCII letters, numbers, dots, dashes, or underscores.

A successful response has this shape:

```json
{
  "schema_version": 1,
  "network": "btc09-mainnet",
  "job_id": "32-lowercase-hex-characters",
  "height": 7883,
  "header_hex": "176-lowercase-hex-characters",
  "target_hex": "64-lowercase-hex-characters",
  "expires_at": "2026-07-13T03:30:00Z",
  "argon_mem_kib": 65536,
  "argon_time": 1
}
```

The 88-byte header uses BTC09's canonical serialized header format. The nonce
is the final eight bytes, encoded little-endian, and is zero in issued work.
Everything except those eight nonce bytes is coordinator-owned and immutable.

Before hashing, a client must verify the schema and network, decode a 16-byte
job ID, decode exactly 88 header bytes, require a zero nonce, derive the target
from the header difficulty bits, compare the advertised target, check the
network's Argon2 parameters, and reject expired work.

## Submit a winning nonce

`POST /api/v1/submit`

```json
{
  "job_id": "32-lowercase-hex-characters",
  "nonce": 18446744073709551615
}
```

`nonce` is an unsigned 64-bit integer. It must be serialized as a JSON number,
not a quoted decimal string.

The coordinator reconstructs its stored block template, inserts the nonce,
checks proof of work, validates the complete block through the normal consensus
path, and broadcasts an accepted block. Success is:

```json
{
  "schema_version": 1,
  "network": "btc09-mainnet",
  "status": "block_accepted",
  "block_id": "64-lowercase-hex-characters",
  "height": 7883
}
```

## Errors

Errors expose a stable code without internal details:

```json
{
  "schema_version": 1,
  "error_code": "low_difficulty"
}
```

Relevant codes are `bad_request`, `unsupported_media_type`, `request_too_large`,
`rate_limited`, `unknown_job`, `expired_job`, `stale_job`,
`duplicate_submission`, `low_difficulty`, `unavailable`, and `internal_error`.
HTTP status remains authoritative. Clients may replace expired, stale, and
unknown jobs immediately; temporary transport, `429`, and `5xx` failures should
use bounded backoff.

## Command-line client

```text
btc09 mine-pool -pool https://mine.btc09.org -address YOUR_09C_ADDRESS -worker home-pc
```

An independent synced node can run a loopback coordinator for its own TLS
reverse proxy:

```text
btc09 node -solo-api 127.0.0.1:9010
```

Never bind the coordinator directly to a public interface. The reference nginx
configuration exposes only the two exact POST routes and adds body, connection,
request-rate, and upstream timeout limits.

## Trust boundary

The coordinator learns the public payout address, worker label, IP address, and
normal request timing. It never needs a private key, seed phrase, wallet file,
or signed spend. The miner must still distrust returned work and validate every
consensus-relevant field before hashing.

Stratum and Stratum V2 model pool channels and partial shares. Open Mining
Protocol v1 deliberately does not claim compatibility with them. A future
pooled service would need separate share difficulty, accounting, block-maturity,
payout, restart, reorg, and operator-policy design.
