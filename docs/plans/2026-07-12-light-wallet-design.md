# BTC09 Light Wallet Design

## Goal

Make the BTC09 desktop wallet behave like a normal consumer wallet: it should open quickly, show spendable funds, receive 09C, and send 09C without requiring the user to download or understand the full chain.

## Chosen architecture

The app defaults to **Fast mode**, backed by an HTTPS wallet gateway on the main BTC09 VPS. Wallet creation, addresses, private keys, transaction construction, and signing stay on the user's device. The gateway exposes only bounded chain queries and accepts already-signed transaction bytes for validation and relay. It never receives a wallet file, private key, or seed material.

The existing local chain and P2P path remains available as **Full node mode** for users who prefer independent verification. The dedicated seed VPS remains P2P-only; the wallet gateway runs beside the main node and is exposed through the existing HTTPS reverse proxy.

## Gateway API

The first version provides three small versioned operations:

- `POST /api/wallet/v1/snapshot`: accepts a canonical, duplicate-free list of BTC09 addresses and returns one atomic tip plus sorted mature, unspent outputs owned by those addresses.
- `POST /api/wallet/v1/broadcast`: accepts one bounded lowercase transaction hex string and its expected transaction ID, decodes it canonically, validates it against the node, admits it idempotently, and relays it to peers.
- `GET /api/wallet/v1/transaction/{txid}`: reuses transaction status semantics so the app can report pending and confirmed payments.

Requests and responses have strict body, address-count, transaction-size, and response-size limits. The process binds to loopback. Nginx and Cloudflare provide TLS, request-rate limits, and public-path routing.

## Client data flow

On startup, the app opens only the local wallet file and sends its public addresses to the gateway. It validates the response network, tip, canonical address order, ownership of every output, money ranges, unique outpoints, and deterministic ordering. It then derives the spendable balance locally.

For a payment, the app selects outputs and signs locally while holding the wallet lock. The confirmation screen is derived from the signed transaction, not from server-supplied display data. On confirmation, the app sends only the signed bytes and expected transaction ID to the gateway. A transient relay failure keeps the preview retryable. An already-admitted transaction is treated idempotently.

Receiving never requires the gateway: the address and QR code come from the local wallet. Gateway downtime cannot lose funds or expose keys; it only delays balance refresh and sending.

## Trust and privacy

Fast mode is non-custodial, not trustless. The gateway can observe queried addresses, omit outputs, report stale data, or refuse relay. It cannot forge a valid spend or redirect funds because the device constructs, checks, and signs the transaction. The UI states this plainly in advanced settings and offers Full node mode as the independent alternative.

The client pins the expected mainnet network identity and rejects malformed, mixed-tip, duplicate, oversized, or noncanonical responses. Future releases can add multiple independently operated gateways without changing the signing boundary.

## User experience

The main wallet remains compact and account-focused. A small status control shows `Fast` or `Full node`, connection state, and last block height. Fast mode is the default on mainnet and refreshes automatically. Full node mode keeps the current peer and sync indicators. Errors use direct language such as `Wallet service is temporarily unavailable. Your funds are safe; try again.`

## Verification

Automated tests cover request bounds, canonical snapshots, malicious gateway responses, local-only signing, exact transaction confirmation, idempotent relay, retry behavior, mode selection, and UI contracts. Release verification includes the full Go test suite, race tests for affected packages, vet, vulnerability scanning, cross-platform builds, browser inspection at desktop and mobile widths, VPS health checks, and public API read-back through HTTPS.
