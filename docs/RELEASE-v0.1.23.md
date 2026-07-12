# Bitcoin 09 v0.1.23

This release adds the first BTC09 desktop wallet for Windows, macOS, and Linux.

## Desktop wallet

- Open the BTC09 binary with no command, or run `btc09 app`.
- Create or use the existing wallet without terminal commands.
- See local balance, chain height, connection state, and peer count.
- Receive with a copyable address and locally generated QR code.
- Create additional receive addresses.
- Make validated offline backup copies without overwriting an existing file.
- Preview destination, amount, fee, total, chain height, and check code before broadcast.
- Retry an unchanged prepared payment after a temporary peer failure.

The interface is embedded in the same executable. It binds only to a random
`127.0.0.1` port, uses one-time launch authentication plus an HttpOnly session,
checks Origin and CSRF on every wallet mutation, loads no remote scripts or web
assets, and never returns private key material through the API.

## Compatibility

There are no consensus, address, wallet-file, P2P protocol, or emission changes.
The desktop app uses the same wallet and chain data as the command-line node.

Expected version output:

```text
Bitcoin 09 (09C) reference node v0.1.23
```
