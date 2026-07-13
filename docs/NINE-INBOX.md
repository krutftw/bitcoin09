# Nine Inbox

Nine Inbox is a small cross-device inbox at https://btc09.org/inbox/. It moves
notes, links, photos, and files between your own browsers. It does not need an account. It does not need 09C.

## Pair another device

Create an inbox on the first device, then open the pairing screen. Scan its QR
code with the second device or copy the pairing link. Compare the two safety
words before finishing. The pairing link contains the inbox encryption key and
write credentials, so treat it like a password: send it only to your own device
and close it when pairing is complete.

Pairing is accountless. There is no email address, phone number, wallet address,
or password-reset service. Each paired browser keeps the inbox secrets in its
local browser storage.

## What is encrypted

The browser encrypts each item with AES-256-GCM before upload. The item is bound
to its inbox and item ID so a ciphertext cannot be silently moved to another
location. Notes, links, file bytes, filenames, and content types are inside the
encrypted envelope. A fresh nonce is used for every item.

The server stores ciphertext and the minimum routing metadata needed to run the
relay. The server can see the inbox and item identifiers, encrypted byte sizes,
creation and expiry times, retention choice, request timing, and network
metadata such as an IP address. It cannot read item contents or filenames
without the key from a paired device.

Nine Inbox is not a replacement for an end-to-end audited messenger or a
permanent backup. Browser compromise, a malicious extension, or anyone who gets
the pairing link can read the inbox. The hosted web code can also change in a
future release; sensitive long-term secrets belong in a purpose-built password
manager or encrypted archive.

## Limits and expiry

- Each item can contain up to 20 MiB of plaintext content.
- An inbox holds at most 50 MiB or 200 live items.
- Normal notes, links, photos, and files expire after seven days.
- Pinned text and links can remain for 30 days. Large files cannot be pinned.
- Deleting an item removes it from the relay and from the current browser's
  local cache. Other devices remove their cached copy when they next sync.

Expiry is part of normal operation, not a backup policy. Keep your own copy of
anything you need later.

## Recovery and deletion

Settings can export an encrypted recovery file. Choose a long password that you
do not reuse. The file contains the pairing secrets encrypted in the browser;
the service cannot reset the password or reconstruct the secrets for you.

"Forget this device" removes the local pairing and decrypted cache from that
browser. It does not delete the shared inbox. A paired device with the recovery
credential can delete the shared inbox; that operation is permanent.

## Background delivery

This version syncs while the page is open and checks again when the page becomes
visible. Installable browser support and share-sheet behavior vary by platform.
Background delivery is not guaranteed, and Nine Inbox does not claim push
notifications when the browser is closed.

## Running a relay

The reference binary includes the loopback-only service used by btc09.org:

```text
btc09 nine-inbox -listen 127.0.0.1:8020 -data-dir /var/lib/btc09-nine-inbox
```

Do not bind it directly to a public interface. The production templates under
`deploy/` put a hardened service behind nginx, enforce body and request limits,
and leave the relay port closed at the cloud firewall.
