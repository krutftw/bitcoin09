# Nine Inbox: Product and System Design

**Date:** 2026-07-13  
**Status:** Approved for implementation by project owner delegation  
**First public location:** `https://btc09.org/inbox/`

## Decision

Build **Nine Inbox** first: a private send-to-self inbox for text, links, photos,
and small documents. A person pairs their phone and computer with a QR code,
then deliberately sends an item from either device. It appears in the same
encrypted history on the other device. No Google, Apple, Microsoft, social,
BTC09, or Nine account is required.

The product promise is: **Send yourself anything. Find it on every device.**
It replaces emailing or messaging yourself, temporary notes, and unreliable
cross-platform clipboard tools. BTC09 remains an optional wallet module and a
source of future payment notifications, not a requirement in the utility flow.

The repository branch keeps its original `nine-send` name, but user-facing
product and new package names are Nine Inbox and `nineinbox`.

## Evidence and decision quality

Vendor pages and press roundups were not treated as demand evidence. The
decision weighs verified store data, open issue trackers, direct platform
documentation, and repeated independent community complaints.

The file-transfer market is already served. LocalSend reports more than five
million downloads and works on all major platforms on one LAN. Blip has more
than one million verified Android installs, a 4.7 rating from more than two
thousand reviews, direct local and remote transfer, original-quality files,
and no size limit. PairDrop provides no-install browser transfer, public rooms,
and persistent pairing. A 20 MiB expiring-link product would be a weaker copy,
not a useful wedge.

The repeated gap is cross-device handoff with history. Users looking for a
Pushbullet replacement consistently ask for cross-platform text, links, files,
and clipboard handoff that works across networks without persistent battery
drain. KDE Connect users value clipboard and file handoff but report discovery,
firewall, reliability, and missing-history friction. Some fall back to Telegram
Saved Messages, Signal, email, or shared documents. Independent comments also
show that automatic clipboard collection creates a trust problem: people want
to choose what leaves a device.

CtrlV validates the exact workflow with a small current product, but its store
presence is early and it requires an account. Nine Inbox differentiates through
accountless QR pairing, open source, self-hostability, explicit sending rather
than clipboard surveillance, and a BTC09 wallet module that can later publish
payment activity into the same private inbox.

## Approaches considered

1. **General utility super-app.** Rejected. Files, clipboard, vault, chat,
   notifications, wallet, mining, trade, and remote control in one release
   create no clear reason to install it.
2. **Large-file transfer app.** Rejected as the first wedge. Mature products
   already offer direct, resumable, unlimited transfers and native clients.
3. **Private paired inbox.** Selected. It solves a repeated daily behavior,
   has a crisp boundary, works as a PWA before native clients exist, and creates
   useful infrastructure for later device notifications and BTC09 activity.

## User experience

The first device opens Nine Inbox and selects **Create my inbox**. The app
generates the encryption material locally, creates an opaque relay mailbox, and
shows a pairing QR code plus a short recovery phrase export. A second device
scans the QR code and confirms the two matching safety words shown on both
screens. The QR fragment carries the mailbox credentials and encryption key;
the fragment is not sent in the initial HTTP request.

The default screen is a chronological inbox and a single **Send something**
composer. The composer accepts text, a pasted link, one photo, or one document
up to 20 MiB. The sender can target all paired devices or keep the item only in
history. Items show honest kinds and actions: Copy for text, Open for links,
Preview or Download for files, Pin, and Delete. Search is local over decrypted
items. The server never receives search terms or plaintext indexes.

The history holds up to 200 items or 50 MiB per inbox and defaults to seven-day
expiry. Pinned text and links may be retained for 30 days; files are never
pinned beyond seven days in version one. A storage meter makes the limit clear.

The PWA installs from supported desktop and mobile browsers and registers as a
share target where available. Version one synchronizes when open or resumed;
it does not promise background delivery or push notifications. Native clients
and real push are a later milestone if people repeatedly use the web product.

## Visual direction and copy

Nine Inbox is a calm personal utility, not a crypto dashboard. It uses near-
black ink, cool white, pale steel, and one signal-orange accent. The core visual
is a narrow, tactile inbox stream with compact items and a composer that expands
only when used. Type is readable and balanced at 390x844 and 1280x800; headings
do not consume the viewport.

There are no gradients, glass cards, crypto charts, fake testimonials, floating
coins, oversized claims, or generic feature-card rows. Motion communicates
pairing, encryption, arrival, and deletion, and respects reduced-motion. Copy is
short and literal: "Send yourself something," "Encrypted on this device," and
"Open Nine Inbox on your other device." Technical terms live in a disclosure.

Tap targets are at least 44 pixels. Focus is visible. Status never relies on
color alone. The interface does not claim virus scanning, anonymity, instant
background delivery, or guarantees it does not provide.

## Pairing and cryptographic model

The first device generates three independent random values using
`crypto.getRandomValues`: a 256-bit AES-GCM encryption key, a 256-bit mailbox
write token, and a 128-bit mailbox recovery token. The relay stores a hash of
the write token and a hash of the recovery token, never the AES key. The pairing
bundle contains mailbox ID, API base, encryption key, write token, and recovery
token. It is encoded only after `#` in the pairing URL and QR code.

Each item uses the inbox AES key with a fresh random 96-bit nonce. The encrypted
JSON envelope contains version, item kind, filename, media type, byte length,
sender device label, created time, expiry, and content. Mailbox ID and item ID
are authenticated as AES-GCM additional data. The relay stores only ciphertext,
nonce, timestamps, counters, and opaque IDs.

Pairing safety words are derived locally from a digest of the AES key and both
device challenges. They are a human check against scanning the wrong code, not
a substitute for HTTPS. Third-party scripts are forbidden. The PWA requires
HTTPS because Web Crypto is restricted to secure contexts.

Version one stores inbox secrets in IndexedDB. It offers an explicit encrypted
recovery file export and warns that clearing browser data removes the device.
Automatic clipboard reading is forbidden. Password-manager content receives no
special access or collection path.

## Relay API and storage

The Go relay listens on numeric loopback only and is exposed through the
existing Cloudflare and nginx site:

- `POST /api/nine/v1/inboxes` creates an inbox from client-supplied token hashes
  and returns a random mailbox ID plus accepted limits.
- `GET /api/nine/v1/inboxes/{id}` returns opaque item headers after write-token
  authentication.
- `POST /api/nine/v1/inboxes/{id}/items` atomically stores one encrypted item.
- `GET /api/nine/v1/inboxes/{id}/items/{item}` returns its ciphertext.
- `DELETE /api/nine/v1/inboxes/{id}/items/{item}` removes one item.
- `DELETE /api/nine/v1/inboxes/{id}` removes the whole inbox using the recovery
  token.
- `GET /healthz` reports readiness without mailbox contents.

Public IDs contain at least 128 random bits. Tokens are checked with constant-
time hash comparison. The API never accepts an encryption key. Failed or
interrupted writes remain invisible. Fixed JSON error codes cover malformed,
unauthorized, missing, expired, oversized, full, and internal states.

Ciphertext and metadata use server-selected filenames, temporary writes, file
sync, and atomic rename. Startup reconciles orphaned or expired records and
removes stale temporary files without following symlinks. A sweeper enforces
item expiry, per-inbox limits, the 2 GiB total service cap, and incomplete-
upload expiry.

## Abuse, privacy, and operations

Accountless storage can be abused even when the server cannot read content.
The initial limits are deliberately conservative: 20 MiB per item, 50 MiB and
200 items per inbox, seven days for files, 30 days only for pinned text/links,
five inbox creations per IP per day, bounded concurrent uploads, and 2 GiB total
ciphertext. Nginx adds per-IP body, request, connection, and method limits.

The relay logs request ID, route class, result, duration, and byte count. It
does not log tokens, URL fragments, ciphertext bodies, decrypted filenames,
text, media types, device names, or search. The service is isolated by systemd
and can be disabled without affecting the node, explorer, wallet gateway, OTC
bot, website, or Discord services.

Deletion is real: item deletion removes its blob and metadata, while inbox
deletion removes the entire mailbox. Backups for this service are disabled in
version one so deleted or expired ciphertext is not silently retained. The
privacy page states that network metadata and operational logs still exist.

## Testing and release gate

Go tests cover token hashing, lifecycle, atomic writes, quota and count limits,
expiry, deletion, restart recovery, symlink and traversal rejection, concurrent
writes, headers, and fixed API errors. JavaScript tests use Node Web Crypto for
pairing-bundle round trips, text and binary envelopes, wrong key, tamper, wrong
mailbox/item binding, oversize input, and malformed data. Browser tests cover
first device, QR pairing, compose, sync, search, copy, link, download, delete,
recovery export, full quota, offline, and expired states.

The release gate includes all Go tests and vet, race tests, JavaScript tests,
vulnerability scan, visual inspection at 1280x800 and 390x844, nginx validation,
live two-browser pairing through Cloudflare, ciphertext-at-rest inspection,
service restart recovery, and read-back of every existing BTC09 service. Public
website and Discord copy are published only after that live path succeeds.

## Explicitly deferred

- Native Android, iOS, macOS, and Linux clients
- Push notifications and guaranteed screen-off/background delivery
- Automatic or continuous clipboard collection
- Large, resumable, directory, or LAN-direct transfer
- SMS mirroring, phone notifications, remote control, chat, cloud drive, VPN,
  password management, or AI features
- BTC09 payment requirements, token gates, paid tiers, or ads

Usage decides the next milestone. Repeat web use supports native clients; demand
for immediate delivery supports push; large-file failure supports a direct
transfer engine. None is prebuilt on speculation.
