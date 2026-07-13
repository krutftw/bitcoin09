# Wallet V2 recovery and encryption

## Goal

Make a new BTC09 wallet behave like a normal self-custody wallet: one recovery
phrase restores every deterministically derived key, the wallet file is
password-encrypted, and an existing V1 wallet is never rewritten or discarded
under the pretense that its random keys came from that phrase.

## Current problem

Wallet V1 stores each Ed25519 seed independently in JSON. Creating a receive
address generates another unrelated seed. A backup only contains the addresses
that existed when that copy was made, so an older backup cannot recover later
addresses. The desktop app has no phrase restore flow and the file is not
password-encrypted.

## New-wallet format

New Wallet V2 files use a strict JSON envelope:

- `schema_version`: `2`
- `network`: the exact BTC09 machine network ID
- `kdf`: Argon2id name, salt, memory, time, and parallelism
- `cipher`: XChaCha20-Poly1305 name, nonce, and ciphertext

The encrypted payload contains 256 bits of recovery entropy, the active address
count, and the derivation identifier. The recovery words, entropy, derived seed,
and private keys never appear in the cleartext envelope. Network and KDF fields
are authenticated as associated data. Parsers reject unknown fields, duplicate
fields, oversized files, unsafe KDF parameters, wrong networks, and trailing
data before key material is accepted.

The first release uses Argon2id with a 64 MiB memory cost, three passes, and
bounded parallelism. Parameters remain in the file so a later release can raise
the cost without breaking old wallets. XChaCha20-Poly1305 supplies authenticated
encryption with a 192-bit random nonce.

## Recovery and derivation

Recovery phrases use 256 bits of entropy and the 24-word English BIP39 format.
BTC09 only generates the English list because that is the interoperable list
recommended by the BIP39 specification. The file password encrypts the local
file; it is not a hidden BIP39 passphrase and is not needed when restoring from
the 24 words.

The BIP39 seed feeds SLIP-0010 Ed25519 hardened derivation. BTC09 fixes these
paths and publishes test vectors:

- mainnet address `i`: `m/9009'/0'/0'/i'`
- regtest address `i`: `m/9009'/1'/0'/i'`

`9009'` is a BTC09 application namespace, not a claimed SLIP-0044 coin-type
registration. All components are hardened because SLIP-0010 does not define
normal public-child derivation for Ed25519.

Restore begins at address zero and scans forward in bounded windows until the
configured unused-address gap is reached. The encrypted file records progress,
but the phrase remains sufficient because the scan can reconstruct it. The
same phrase and network must always produce the same address sequence on every
supported platform.

Primary references:

- BIP39: https://github.com/bitcoin/bips/blob/master/bip-0039.mediawiki
- SLIP-0010: https://github.com/satoshilabs/slips/blob/master/slip-0010.md

## Password and unlock behavior

The app asks for a password when creating or restoring a V2 wallet and asks for
it again after restart. It never sends that password to the gateway, explorer,
Discord, or mining coordinator. A wrong password, modified ciphertext, or
modified authenticated metadata returns one generic unlock failure so the file
does not become a password oracle.

The app must show the recovery phrase once during setup, require a small word
confirmation, and strongly recommend paper or another offline copy. Clipboard
copy is optional and carries a visible warning. Screenshots, logs, telemetry,
support reports, and browser storage must never contain the phrase.

## Legacy wallet migration

V1 and pre-V1 wallets remain readable. There is no in-place conversion because
their independently random keys cannot be recreated from a new phrase.

The migration flow is deliberately non-destructive:

1. Open and back up the legacy wallet.
2. Create a separate V2 wallet and confirm its 24-word recovery phrase.
3. Stop legacy mining and switch the payout address to V2.
4. Sweep mature spendable outputs to V2 after an explicit fee and destination
   review.
5. Keep the legacy backup for immature mining rewards and any addresses that
   may receive later payments.

The UI must never say that the V2 phrase restores legacy keys. A legacy wallet
continues to show a file-backup warning until all relevant funds and future
payouts have moved.

## Release gates

- Official BIP39 and SLIP-0010 vectors pass.
- Create, reopen, add-address, and phrase-restore sequences match byte-for-byte.
- Wrong passwords, tampering, wrong networks, malformed JSON, hostile KDF
  settings, symlinks, hard links, crashes, and concurrent writers fail safely.
- Wallet files contain no plaintext phrase, entropy, seed, or derived private
  key.
- Windows, macOS, and Linux builds restore the same published address vectors.
- The desktop create/restore/unlock flow passes browser tests and a visual pass.
- Legacy migration is tested with immature and mature rewards before release.
