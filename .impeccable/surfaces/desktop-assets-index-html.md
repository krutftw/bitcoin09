---
version: 1
slug: "desktop-assets-index-html"
primary_target: "desktop/assets/index.html"
related_targets: ["desktop/assets/app.css","desktop/assets/app.js","desktop/assets/miner.css","desktop/assets/miner.js","tools/desktop/native-smoke.mjs"]
---

# BTC09 Bundled Desktop Wallet Surface

- **Scope / mode:** The core-hosted surface packaged inside the Windows, macOS, and Linux Tauri wallet. Operate.
- **Audience / job:** Desktop users creating or restoring an encrypted wallet, checking funds, receiving, reviewing and sending payments, reading history, combining small payments, backing up, and optionally mining from the full client.
- **Primary task:** Keep the wallet state and next safe action obvious without hiding desktop evidence or reducing the application to a phone-sized column.
- **Evidence / content:** Use only authenticated loopback-core status, wallet, activity, preview, cleanup, backup, and miner data. Do not invent price, exchange, latency, or portfolio data.
- **Chosen direction:** The Inspectable Instrument. The bundled desktop UI uses the same solder-mask rail, copper square mark, flat trace panels, restrained typography, and explicit review hierarchy as the Android/iPhone surface.
- **Memorable moment:** The balance and action bay reads as one instrument, followed by a flat ledger where receive, send, activity, cleanup, backup, and mining retain their full desktop capabilities.
- **Constraints:** Preserve loopback token authentication, CSRF, navigation policy, wallet-only build stripping, recovery confirmation, local password encryption, 48 px primary controls, readable identifiers, and the existing core API.
- **Verification:** `tools/desktop/native-smoke.mjs` must exercise the embedded assets from a freshly built wallet-only core, assert the instrument geometry and colors, capture onboarding/receive/send/activity, and confirm the miner surface is absent.
