# Nine Send: Product and System Design

**Date:** 2026-07-13  
**Status:** Approved for implementation by project owner delegation  
**First public location:** `https://btc09.org/send/`

## Decision

Build **Nine Send** first: an installable web app that moves text, links, photos,
and small documents between phones and computers through an encrypted,
expiring link. The sender opens the page, chooses content, and gets a link or QR
code. The recipient needs only a browser. No account, BTC09 balance, browser
extension, or matching device ecosystem is required.

This is the narrowest product that is useful to people who do not care about
cryptocurrency while creating infrastructure that the BTC09 desktop wallet can
use later. BTC09 remains an optional module and provenance, not an obstacle in
the transfer flow.

The name is provisional at the company/product-family level but final for this
release. "Nine Send" is clearer and more searchable than the generic "Nine,"
while leaving room for later modules such as Nine Inbox and the BTC09 Wallet.

## Research basis and rejected approaches

LocalSend proves that cross-platform, no-account transfer is a real mainstream
need: its official site reports more than five million downloads and support
for Windows, macOS, Linux, Android, and iOS. Its explicit boundary is the local
Wi-Fi network. Apple's Universal Clipboard is smooth but requires nearby Apple
devices on one Apple Account. Microsoft Phone Link similarly depends on a
Microsoft account and varies by phone. PairDrop reaches across networks, but
its strongest flows still assume both peers are present. Bitwarden Send proves
that encrypted expiring links work for recipients without accounts, though the
feature lives inside a password-manager workflow.

Three directions were considered:

1. **Large utility super-app.** Rejected because files, clipboard, vault,
   notifications, chat, wallet, mining, and trade would produce a confusing
   first release with no single reason to install it.
2. **Native Windows and Android device bridge.** This remains the medium-term
   direction, but it delays public usefulness behind two installers, mobile
   store distribution, background-service work, and native pairing.
3. **Encrypted send PWA.** Selected because it works immediately on desktop
   and mobile, can be installed from a browser, can receive shared content on
   supported platforms, and establishes the encryption, relay, and inbox data
   model needed by native clients later.

## User experience

The default screen has one dominant action area rather than a dashboard. A
person can drop a file, choose a photo, paste a link, or type text. They choose
an expiry of one hour or 24 hours, then press **Create private link**. Encryption
happens on the device before upload. The result screen shows a QR code, a copy
button, a native share button when available, the expiry, and the plain warning
that anyone with the complete link can open the item.

Opening a receive link decrypts locally. Text and links render in a restrained
preview with Copy or Open. Files show the real filename, size, and Download.
The interface never claims a scan, identity check, or guarantee it does not
perform. It tells recipients to open files only when they trust the sender.

The PWA is installable, caches only its application shell, and registers as a
share target where the browser supports that capability. Unsupported browsers
still get the full website flow. The app does not require installation and does
not nag after a declined install prompt.

The desktop wallet gains a quiet **Send files & text** link to Nine Send. The
BTC09 website gets a concise utilities section. Neither surface suggests that
09C is needed to use the service.

## Visual direction and copy

Nine Send is a calm consumer utility, not a crypto dashboard. The visual system
uses near-black ink, cool white, pale steel, and one signal-orange accent. Type
is compact and readable; headings are prominent without filling the viewport.
The memorable element is a large physical-looking transfer tray whose state
changes from empty, to encrypting, to ready. Motion is limited to state changes
and respects reduced-motion preferences.

There are no gradients, glass cards, crypto charts, fake testimonials, floating
coins, oversized marketing claims, or three-column feature-card filler. Copy is
short and literal: "Send something," "Encrypted on this device," and "Expires
in 24 hours." Terms such as AES-GCM and ciphertext live in a small technical
details disclosure instead of the primary flow.

Layouts are designed at 390x844 and 1280x800 first, then checked at intermediate
widths. Tap targets are at least 44 pixels. Status and errors use text and icons
in addition to color. Keyboard focus is always visible.

## Cryptographic envelope

The browser creates a random 256-bit AES-GCM key and a random 96-bit nonce using
`crypto.getRandomValues`. A compact JSON payload contains version, content kind,
filename, media type, byte length, and either UTF-8 text or base64-encoded file
bytes. The payload is encoded and encrypted with AES-256-GCM. Version and drop
ID are authenticated as additional data.

The relay receives only the encrypted blob, nonce, expiry, public drop ID, and
operational counters. The key is placed after `#` in the receive URL. URL
fragments are not included in HTTP requests, so the relay and normal access
logs never receive the key. The recipient fetches ciphertext, decrypts in the
browser, validates the envelope and declared length, then renders or downloads
it. Authentication failure produces a generic invalid-or-damaged message.

Each encryption operation uses a fresh key and nonce. No password-derived keys,
home-grown cipher, server-side decryption, or reusable master key is included.
The app is served only over HTTPS because Web Crypto is restricted to secure
contexts. Third-party scripts are forbidden so the encryption boundary remains
auditable.

Version one deliberately caps plaintext at 20 MiB. The browser must hold the
payload and encrypted result briefly in memory, so the interface checks size
before reading. Chunked encryption is reserved for a native-client milestone.

## Relay API and storage

The Go relay listens on numeric loopback only and is published through the
existing Cloudflare and nginx path. Its API is:

- `POST /api/nine/v1/drops` creates an empty drop and returns a random ID,
  single-use upload token, and accepted limits.
- `PUT /api/nine/v1/drops/{id}` accepts one opaque encrypted blob with the
  upload token. A failed or interrupted upload never becomes readable.
- `GET /api/nine/v1/drops/{id}` returns the immutable encrypted blob and
  expiry metadata, with a maximum of five successful fetches.
- `DELETE /api/nine/v1/drops/{id}` removes a drop when presented with the
  creator token.
- `GET /healthz` reports process readiness without storage contents.

IDs and tokens contain at least 128 random bits. Error responses are fixed JSON
objects and do not reveal whether a guessed ID existed where that distinction
is unnecessary. Creation, upload, and fetch have separate IP limits at nginx.
The application additionally limits concurrent uploads, request bodies, drop
count, total stored bytes, and expiry to one or 24 hours.

Ciphertext and small metadata records are written to a dedicated data directory
using temporary files, file synchronization, and atomic rename. A sweeper
deletes expired or incomplete records. Startup reconciles metadata with blobs
and removes stale temporary files. The process never follows symlinks and uses
random server-selected filenames rather than user input.

## Failure handling and operations

The sender keeps the plaintext selection in the page if creation or upload
fails, allowing a retry without reselecting it. Offline, quota, expiry, missing,
oversized, and damaged-link states have different human messages but stable API
codes. The recipient never receives a half-written upload.

The relay emits structured logs containing request ID, route, result code,
duration, and byte count. It does not log tokens, fragments, filenames, text,
media types from the encrypted envelope, or request bodies. Health checks and
disk usage are observable. Deployment includes a systemd sandbox, loopback-only
bind, nginx body and rate limits, and a total storage ceiling. The relay can be
disabled without affecting the node, explorer, wallet gateway, OTC bot, or site.

Deployment starts with a 20 MiB item limit, 2 GiB total ciphertext cap, five
downloads per item, and 24-hour maximum expiry. These limits are intentionally
conservative after the recent denial-of-service event. Usage data, errors, and
abuse reports determine whether limits rise; optimism does not.

## Testing and release gate

Go tests cover random identifiers, lifecycle transitions, atomic persistence,
expiry, quota enforcement, interrupted uploads, restart recovery, traversal
resistance, response headers, and fixed error contracts. JavaScript tests run
the same envelope code under Node Web Crypto and prove round-trip, wrong-key,
tamper, oversize, and malformed-envelope behavior. Browser tests exercise the
real send and receive flows at desktop and mobile sizes.

The release gate is: `go test ./...`, `go vet ./...`, JavaScript tests, a race
run for the relay package, vulnerability scan, static asset contract tests,
visual inspection at 1280x800 and 390x844, nginx configuration test, live upload
and download through Cloudflare, ciphertext inspection proving no plaintext or
filename at rest, service restart recovery, and health checks for all existing
BTC09 services. Website and Discord copy are published only after the live flow
passes from a clean browser.

## Explicitly deferred

- Native Android, iOS, macOS, and Linux shells
- Background paired-device inbox and push notifications
- Large-file chunking, resumable uploads, and LAN-direct transfer
- Accounts, contact lists, cloud drive, chat, password management, or VPN
- BTC09 payments for ordinary use, paid tiers, token gates, or ads
- Automatic clipboard collection

These are not promises for the first release. The next milestone is chosen from
observed use: paired inbox if repeat send-to-self dominates, native share-sheet
clients if installation demand is real, or large direct transfer if the 20 MiB
ceiling is the main failure.
