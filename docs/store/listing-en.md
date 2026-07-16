# BTC09 Wallet store listing

## Product name

BTC09 Wallet

## Short description

A self-custody Bitcoin 09 wallet for sending and receiving 09C.

## Description

BTC09 Wallet is a straightforward way to use Bitcoin 09 without a terminal.

- Create or restore a wallet.
- Send and receive 09C.
- Review the address, amount and fee before sending.
- See recent wallet activity and confirmations.
- Scan a recipient QR code on Android and iPhone.
- Lock the wallet when you are finished.

No account is required. Your recovery words and private keys stay on your device. The app uses the public Bitcoin 09 network to read balances and broadcast transactions.

This is a self-custody wallet. Bitcoin 09 contributors cannot recover lost recovery words, unlock a wallet, reverse a payment, or promise that 09C will have a market value.

BTC09 Wallet is not an exchange and does not buy or sell cryptocurrency. The Android, iPhone and Microsoft Store editions do not mine cryptocurrency on the device.

Privacy: https://btc09.org/privacy.html

Source and support: https://github.com/krutftw/bitcoin09

## Feature bullets

- Create or restore a self-custody wallet
- Send and receive 09C
- Review every payment before broadcast
- Scan recipient QR codes on mobile
- See confirmations and recent activity

## Search terms

BTC09, Bitcoin 09, 09C, self-custody wallet

## What is new in 0.1.33

The first native mobile wallet adds protected on-device storage, wallet restore, send and receive, activity, payment review, and recipient QR scanning. The Windows Store edition removes on-device mining and uses the same simpler wallet flow.

## Notes for review

- BTC09 Wallet is a non-custodial software wallet for the open-source Bitcoin 09 network.
- It has no sign-in, subscription, in-app purchase, exchange, fiat payment, custody, staking, rewards, or advertising.
- A reviewer can create a disposable wallet without a test account. The recovery words shown during setup are for that disposable wallet only.
- Private keys, recovery words and unlock codes stay on the device. Public addresses are sent over HTTPS to retrieve public chain data. A locally signed transaction is sent only after the user confirms it.
- Android and iPhone request camera access only after the user taps Scan QR. Camera frames are decoded on the device and are not uploaded.
- The Android and iPhone apps contain no miner. The Microsoft Store build starts its local wallet core in wallet-only mode and does not expose mining controls.
- Privacy policy: https://btc09.org/privacy.html
- Source code: https://github.com/krutftw/bitcoin09
