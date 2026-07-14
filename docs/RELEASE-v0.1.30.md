# Bitcoin 09 v0.1.30

This release makes the public explorer useful for checking payments without
calling its JSON API by hand.

## Explorer

- Search accepts a block height, TXID, or 09C address.
- Transaction pages show confirmed, mempool, or not-found status.
- Confirmed transactions link to their block and show confirmation count.
- Address pages list every confirmed transaction involving the address, newest
  first.
- Address history combines inputs and change outputs into the net amount
  received or sent by each transaction.
- Coinbase payments to any output are included in mined-block history, which
  also covers multi-output PPLNS payouts.

## Compatibility

There are no consensus, proof-of-work, wallet-format, P2P, supply, or mining
protocol changes in this release. Existing nodes and wallets remain compatible.
