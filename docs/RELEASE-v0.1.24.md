# Bitcoin 09 v0.1.24

This release makes the desktop wallet practical for everyday use without turning it into a custodial service.

## Desktop wallet

- Mainnet now opens in Fast mode by default. It reads public balance and spendable-output data from the BTC09 HTTPS wallet gateway instead of downloading the full chain.
- Receiving remains completely local: the app creates addresses and QR codes from the wallet file on the device.
- Sending remains non-custodial: the app validates gateway data, constructs the transaction, shows the exact recipient, amount, and fee, and signs on the device. Only the finished signed transaction is sent to the gateway for relay.
- Full node mode remains available with `btc09 app -mode full`.
- The interface now shows the active mode, connection state, and latest block clearly at desktop and phone widths.

## Node operators

The node can expose the light-wallet service on a literal loopback address:

```text
btc09 node -wallet-gateway 127.0.0.1:8010
```

The gateway refuses wildcard and public bind addresses. Public deployment belongs behind TLS, strict request limits, and a reverse proxy. Reference nginx fragments are in `deploy/nginx/bitcoin09-wallet-gateway-*.conf`.

## Security boundary

The gateway receives public wallet addresses and signed transaction bytes. It never receives wallet files or private keys. Fast mode can be censored or given stale public data by a bad gateway, but a gateway cannot sign a payment or change its destination. Users who want independent chain verification can use Full node mode.

## Verification

Release artifacts are accompanied by `SHA256SUMS`. Verify the checksum before running a downloaded binary.
