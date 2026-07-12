# BTC09 Desktop Wallet Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use executing-plans to implement this plan task-by-task.

**Goal:** Ship a secure, wallet-first BTC09 desktop application in the existing single cross-platform executable.

**Architecture:** Add an embedded local web application behind a loopback-only authenticated Go HTTP server. Keep HTTP/session policy in a new `desktop` package and inject a command-package service that reuses the existing durable wallet, persisted chain, transaction preparation, and P2P code.

**Tech Stack:** Go 1.25, standard `net/http` and `embed`, existing BTC09 wallet/core/P2P packages, local HTML/CSS/JavaScript, server-side QR SVG generation.

---

### Task 1: Secure session and HTTP foundation

**Files:**
- Create: `desktop/server.go`
- Create: `desktop/server_test.go`

1. Write failing handler tests proving non-loopback hosts are rejected, the launch token is exchanged for a secure session, the URL is cleaned, and state-changing requests require same-origin plus CSRF.
2. Run `go test ./desktop -run 'Test(Server|Session|Mutation)' -v` and verify the missing package/handler fails.
3. Implement bounded JSON responses, loopback host validation, launch authentication, strict cookie policy, CSRF validation, security headers, and method routing.
4. Re-run the focused tests and verify they pass.
5. Commit the security foundation.

### Task 2: Wallet application API and pending-send state

**Files:**
- Modify: `desktop/server.go`
- Modify: `desktop/server_test.go`
- Create: `desktop/types.go`

1. Write failing tests for status, wallet creation, new receive address, backup, send preview, expired preview, one-time confirmation, replay rejection, and safe public errors.
2. Run `go test ./desktop -v` and verify each new behavior fails for the expected missing route or state.
3. Add a narrow injected `Service` interface, response types, strict request decoding, expiring per-session pending sends, one-time confirmation, and stable public error codes.
4. Re-run `go test ./desktop -v` and verify all handler tests pass.
5. Commit the API layer.

### Task 3: Real BTC09 service integration

**Files:**
- Create: `cmd/btc09/app_service.go`
- Create: `cmd/btc09/app_service_test.go`
- Modify: `cmd/btc09/main.go`

1. Write failing regtest-backed tests for first-run status, explicit wallet creation, durable address creation, safe backup copying, anchored send preview, and broadcast confirmation.
2. Run `go test ./cmd/btc09 -run 'TestAppService' -v` and verify failures are caused by the absent service.
3. Implement the service with existing wallet and chain APIs, canonical paths, private-key-safe responses, short-lived signed transaction storage, exact transaction confirmation, and injected broadcast seam.
4. Re-run the focused command tests and then `go test ./wallet ./core ./p2p ./cmd/btc09`.
5. Commit the real backend integration.

### Task 4: Embedded desktop interface

**Files:**
- Create: `desktop/assets/index.html`
- Create: `desktop/assets/app.css`
- Create: `desktop/assets/app.js`
- Create: `desktop/assets_test.go`
- Modify: `desktop/server.go`

1. Write failing asset contract tests for embedded/offline assets, accessible landmarks and controls, no external resources, first-run/create flow, receive QR/copy/backup actions, send preview/review/confirm flow, loading/error states, and responsive/reduced-motion CSS.
2. Run `go test ./desktop -run 'TestEmbedded|TestInterface' -v` and verify failure because assets are absent.
3. Implement the polished 2009 network-instrument interface and wire it to the authenticated API without exposing session secrets or private key material.
4. Re-run `go test ./desktop -v` and a local browser smoke test at narrow and wide viewport sizes.
5. Commit the interface.

### Task 5: Cross-platform launch, docs, and release gate

**Files:**
- Create: `cmd/btc09/app.go`
- Create: `cmd/btc09/open_browser_windows.go`
- Create: `cmd/btc09/open_browser_darwin.go`
- Create: `cmd/btc09/open_browser_linux.go`
- Create: `cmd/btc09/app_test.go`
- Modify: `cmd/btc09/main.go`
- Modify: `README.md`
- Modify: `QUICKSTART.md`

1. Write failing tests for loopback-only random binding, explicit `app` command routing, safe launch URL construction, shutdown behavior, and default no-argument desktop launch selection.
2. Run `go test ./cmd/btc09 -run 'Test(App|Desktop)' -v` and verify failure for missing launch code.
3. Implement the app runtime, per-platform browser launch, P2P sync startup, signal shutdown, and human-facing docs.
4. Run `gofmt`, `go test ./...`, `go vet ./...`, site/project contracts, race tests where supported, and build Windows amd64, Linux amd64/arm64, and macOS amd64/arm64 binaries.
5. Launch a disposable regtest app, verify the authenticated interface and core workflow over loopback, inspect packaged binaries, and commit the completed app.
