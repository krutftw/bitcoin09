# Bitcoin 09 v0.1.29

This release fixes two setup problems reported in Discord.

## macOS downloads

- Apple silicon and Intel downloads now include `Bitcoin 09.app` inside a ZIP.
- The ZIP preserves the executable permission that was lost when people
  downloaded the raw Mach-O file directly.
- The app package includes a short first-launch guide.
- Current community builds are not Apple-notarized. Verify the ZIP against
  `SHA256SUMS`; if macOS blocks the first launch, right-click the verified app
  and choose **Open**.

Raw macOS binaries remain attached for command-line users, but the website
links to the app ZIPs.

## Discord OTC form

- The payment-method and settlement-network fields now give concrete examples.
- Bank, Wise, and cash trades are told to leave the network field blank.
- Entering an asset code such as USDT into the network field now explains the
  difference and suggests TRC20, ERC20, and other supported networks.
- Normal input rejections are no longer logged as bot system failures.

There are no consensus, proof-of-work, supply, wallet-format, or P2P protocol
changes in this release.
