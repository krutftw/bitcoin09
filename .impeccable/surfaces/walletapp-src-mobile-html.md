---
version: 1
slug: "walletapp-src-mobile-html"
primary_target: "walletapp/src/mobile.html"
related_targets: ["walletapp/src/mobile.css","walletapp/src/mobile.js"]
---

# BTC09 Wallet Surface

- **Scope / mode:** Wallet operations across desktop and phone webviews. Operate.
- **Audience / job:** New and existing 09C holders checking funds, receiving, reviewing and sending payments, reading confirmations, and protecting recovery access without terminal knowledge.
- **Primary task:** Make current wallet state and the next safe payment action obvious. Preserve the complete create, restore, lock, backup, receive, send, activity, and settings flows.
- **Evidence / content:** Use only wallet-plugin status, address, height, activity, preview, and recovery data already available. Do not invent price, peers, latency, portfolio, or exchange data.
- **Chosen direction:** The Inspectable Instrument, composition A. A desktop instrument rail anchors a dominant balance/action field and aligned transaction ledger; on phones it collapses to the established bottom navigation.
- **Memorable moment:** A payment moves from input to an explicit review readout with destination, fee, total, and check code shown like an inspectable instrument result.
- **Constraints:** Self-custody, local assets only, no analytics, no terminal language, 48 px touch targets, keyboard focus, reduced motion, readable identifiers, and no color-only state.
- **Unresolved:** Trusted platform signing and store distribution are release concerns, not interface claims.
