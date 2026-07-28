# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

The wallet UI is delivered inside Tauri on Windows, macOS, Linux, Android, and iOS. It must adapt across desktop and phone-sized webviews without pretending that a webview wrapper is a separate native design system.

## Users

- People receiving, holding, or sending 09C who expect a normal wallet app and should not need a terminal, node, exchange account, or prior cryptocurrency expertise.
- Existing BTC09 miners and technical users who need a dependable way to inspect payments and move mined funds without exposing wallet secrets.
- Inferred and approved under delegated direction: newcomers evaluating whether BTC09 feels usable and trustworthy enough to try.

## Product Purpose

BTC09 Wallet provides the ordinary self-custody payment loop: create or restore a wallet, protect the recovery phrase, see spendable funds, receive 09C, review and send a payment, understand confirmation state, and maintain the wallet.

Success means a first-time user can complete those jobs without command-line knowledge, while an experienced user can verify important details before committing a payment.

## Positioning

BTC09 Wallet is a wallet-only application for the native 09C chain. It does not bolt 09C onto an Ethereum or Solana wallet, hold funds in a project account, require cloud login, or hide the on-chain payment model behind an exchange balance.

## Operating Context

- The app can open on an empty first-run wallet, a locked wallet, a ready wallet, or a temporarily unavailable network.
- Mobile users may scan and share addresses. Desktop users more often paste, copy, and compare details across a wider window.
- Users may have many small mining or payment outputs and can combine those outputs back into the same wallet.
- Coinbase mining rewards remain immature until the chain confirms them sufficiently.
- Windows and macOS direct-download builds can still show publisher warnings while trusted signing is completed.

## Capabilities and Constraints

- Create or restore from 24 recovery words.
- Lock on backgrounding and unlock through the platform security bridge where available.
- Display available and immature balances, sync/network state, wallet address, chain height, and recent activity.
- Generate and share a receive address and QR code.
- Scan, paste, validate, preview, confirm, and broadcast a payment.
- Show the destination, fee, total, and a short check code before broadcast.
- Display sent, received, mining, immature, pending, and confirmed activity states.
- Reveal recovery words only through the explicit backup flow.
- Self-custody boundary: private keys and recovery words stay on the device. The UI must not add analytics, remote fonts, third-party scripts, hidden demo wallets, or a cloud account.
- Wallet-only boundary: the native package does not contain the node or miner. Those remain in the full BTC09 client.
- The existing wallet plugin command contract, CSP, history behavior, automatic background locking, and 48 px minimum touch targets must remain intact.

## Brand Commitments

- Product name: Bitcoin 09 Wallet or BTC09 Wallet. Ticker: 09C.
- Voice is short, direct, calm, and human. It explains consequences without sounding corporate or promotional.
- Preserve the established square `09` mark and the project facts: fair launch, no premine, no company allocation, and no guaranteed value.
- Do not frame 09C as an investment or imply a future price.

## Evidence on Hand

- Working wallet screens and behavior: the Android/iPhone surface in `walletapp/src/mobile.html`, `walletapp/src/mobile.js`, and `walletapp/src/mobile.css`, plus the bundled Windows/macOS/Linux surface in `desktop/assets/`.
- Platform and packaging configuration: `walletapp/src-tauri/`.
- Product behavior documentation: `README.md` and `docs/DIRECT-DOWNLOADS.md`.
- Brand rules and assets: `BRAND.md`, `docs/assets/`, and `walletapp/src-tauri/icons/`.
- Automated interaction, security, and responsive checks: `tools/mobile/ui-smoke.mjs`, `tools/mobile/ui-contract.test.mjs`, and `tools/mobile/native-bridge.test.mjs`.
- No testimonials, adoption figures, fiat price feed, or exchange listing is available and none should be fabricated.

## Product Principles

1. Make the next safe action obvious.
2. Show plain-language status before technical detail.
3. Keep custody and payment consequences visible at the moment they matter.
4. Let desktop users use their space without making the phone layout feel secondary.
5. Prefer dependable, inspectable behavior over decorative novelty.

## Accessibility & Inclusion

- Maintain keyboard operation, visible focus, semantic labels, reduced-motion handling, safe-area support, readable transaction identifiers, and at least 48 px touch targets.
- Do not rely on color alone for network or transaction status.
- Use ordinary English and keep critical instructions understandable without cryptocurrency jargon.
