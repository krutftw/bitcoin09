# BTC09 Desktop Wallet Design

## Product scope

BTC09 Desktop Wallet is the friendly entry point for people who should not need a terminal to use 09C. The first release is deliberately wallet-first: it creates or opens the existing mainnet wallet, shows synchronization and spendable balance, provides receive details with a QR code, prepares and reviews sends, broadcasts approved transactions, and reports recent transaction status. It uses the same wallet file and consensus code as the command-line node, so current users do not need to migrate keys or maintain a second implementation.

The application ships as the existing single BTC09 executable. Running `btc09 app` starts a local-only HTTP service on a random loopback port and opens the system browser. The interface is embedded into the binary, works offline for wallet backup and receive operations, and clearly distinguishes local data from network-confirmed data. The first release targets Windows, macOS, and Linux. Mobile, mining controls, exchange integrations, and built-in fiat trading remain follow-on work because each adds materially different security, distribution, or regulatory concerns.

Success means a newcomer can download BTC09, double-click it, create a wallet, back it up, receive coins, and send a reviewed transaction without entering a command. Existing CLI behavior remains compatible.

## Architecture and security boundaries

A new `app` package owns an `http.Handler` and a small service interface. The command package wires that service to existing chain, wallet, and P2P functionality. Static HTML, CSS, and JavaScript are embedded with Go's `embed` package; no external runtime, CDN, analytics, or remote fonts are required. A random high-entropy session token is placed in the launch URL, exchanged for a strict SameSite cookie, and removed from the visible URL. State-changing requests require that cookie, a matching per-session CSRF header, POST semantics, JSON content types, and an allowed loopback Origin. The server binds only to `127.0.0.1` on an operating-system-selected port and rejects non-loopback request hosts.

The app never returns private keys through its API. Backup is an explicit filesystem copy performed by the backend after the user chooses a destination; the UI shows the source wallet path and plain-language safety guidance. Wallet creation uses the existing durable key store and locking. Sending is split into preview and confirm stages: preview builds a signed transaction against an anchored chain tip, stores it in short-lived in-memory pending state, and returns human-readable outputs and fees. Confirm requires the pending identifier and the same session, then revalidates and broadcasts exactly those bytes. Pending transactions expire and cannot be silently altered by the browser.

## Interface and behavior

The visual direction is a polished 2009 network instrument: warm paper-white surfaces, near-black ink, oxidized copper accents, compact ledger typography, and a subtle dot-grid signal field. It should feel like dependable software made for a machine, not a generic exchange dashboard. The primary screen has a narrow status rail and one dominant wallet ledger. Strong hierarchy keeps the current balance, receive address, and send action obvious without filling the screen with cards.

On first run, the app explains where the wallet is stored and requires an explicit acknowledgment before creating it. After creation, the receive view shows the canonical address, copy button, and QR code generated locally. The send flow validates address and decimal amount as the user types, but the backend remains authoritative. The review sheet names destination, amount, fee, total debit, selected inputs, and chain height before enabling broadcast. Errors are mapped to human messages with a stable support code; sensitive internal error text does not cross the API boundary.

The interface is responsive down to a narrow desktop window and is keyboard accessible. Motion is limited to a single startup reveal, synchronization activity, and clear send-state transitions, with reduced-motion support. All core actions work without third-party services.

## Data flow, failure handling, and testing

At startup the command resolves the standard data directory and mainnet wallet path, creates a session, binds loopback, opens the browser, and serves until interrupted or idle timeout. `GET /api/v1/status` returns application version, network, wallet existence, addresses, chain height and tip, balance, peer state, and synchronization state. Wallet creation and new-address operations are explicit POSTs. Send preview returns an expiring pending ID; confirmation consumes it once and returns the transaction ID and broadcast result. A lightweight status endpoint checks whether recent transactions are known locally or confirmed in the canonical chain.

If no local chain exists, the app remains useful for creating, backing up, and receiving, while send is disabled with a direct explanation. If chain data is stale or changes during preparation, the app refreshes status and asks the user to review again. Network failure never destroys the prepared transaction or marks it sent; the result distinguishes local acceptance from successful peer broadcast. Multiple app instances share wallet and chain locks and receive a clear busy response.

Tests are written first. Handler tests cover loopback host/origin enforcement, token exchange, CSRF, content types, response bounds, redacted errors, first-run state, wallet creation, backup, preview expiry, one-time confirmation, and replay rejection. Service tests use regtest chain data for real wallet preparation and transaction inspection. Browser-facing tests exercise first run, receive, validation, review, failure, and narrow viewport behavior. The existing full Go and project contract suites remain the release gate, followed by cross-compilation and a manual local smoke test of the packaged executable.
