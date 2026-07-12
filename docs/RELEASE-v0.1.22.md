# Bitcoin 09 v0.1.22

This release moves the primary public seed onto its own P2P-only server.

## What changed

- `seed.btc09.org:9009` now points to the dedicated seed.
- `178.128.52.20:9009` is included directly in the built-in mainnet seed list.
- The former Singapore seed and the community peers remain as fallbacks.
- The website and operator documentation now show the current seed layout.
- Plain Discord OTC replies no longer get stuck on “thinking” under discord.py
  2.7 when there is no button view to attach.
- Invalid manual/public order numbers now direct traders to `/trade list` for
  current bot escrow order IDs.

There are no consensus, wallet, transaction, or mining changes in this release.
Older clients remain compatible because the seed hostname has not changed.

## Verify

Download the binary and `SHA256SUMS.txt` from this release, then verify the
checksum before running it. The version command should print:

```text
Bitcoin 09 (09C) reference node v0.1.22
```
