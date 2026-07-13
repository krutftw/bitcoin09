# Bitcoin 09 v0.1.27

This release gives new desktop wallets a normal recovery and unlock flow.

## Recovery wallets

- New wallets create 24 English recovery words and encrypt the local wallet
  file with Argon2id and XChaCha20-Poly1305.
- The app checks three words during setup, locks after restart, rejects an
  incorrect password without exposing file details, and can restore the same
  address from the 24 words.
- Recovery words can be shown again only after the local password is entered.
  They are not saved in browser storage, copied to support reports, or sent to
  the wallet gateway, explorer, miner, or Discord.
- The first V2 desktop release uses one stable receive address. Address
  rotation will return after spent-address history can be recovered safely.
- Encrypted wallet-file backups are still available and are verified after
  they are written.

## Existing wallets

Existing V1 wallets remain readable and byte-for-byte unchanged. Their random
keys cannot honestly be recovered from a newly generated phrase, so the app
keeps their existing file-backup flow. There is no automatic conversion or
fund movement in this release.

## Compatibility and verification

There are no consensus, P2P, mining, emission, or address-format changes. The
recovery derivation is fixed at `m/9009'/0'/0'/0'` on mainnet and
`m/9009'/1'/0'/0'` on regtest using BIP39 and SLIP-0010 Ed25519. Release
artifacts are accompanied by `SHA256SUMS`.
