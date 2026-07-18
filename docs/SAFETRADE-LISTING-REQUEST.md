# SafeTrade listing request

Use the official SafeTrade support form:
https://support.safetrade.com/hc/en-us/requests/new

Do not attach API keys, wallet files, private keys, signed transactions, or
server logs containing credentials.

## Subject

Bitcoin 09 (09C) listing request

## Body

Hello SafeTrade team,

I'd like to submit Bitcoin 09 (09C) for listing review.

09C is a live native UTXO chain with Argon2id proof of work using 64 MiB per
hash attempt. It follows a 21 million coin cap, 50 09C starting block reward,
210,000-block halvings, and a 10-minute target. Release v0.1.34 activated
per-block ASERT difficulty adjustment at height 12,096 with a two-hour
half-life. There was no premine, ICO, team allocation, or treasury allocation.
The genesis reward is unspendable, so circulating supply starts with publicly
mined blocks after genesis.

The reference node is a clean-room Go implementation under the MIT license. It
includes the node, wallet, CPU miner, P2P sync, block explorer, strict JSON
wallet commands, and versioned read-only endpoints for tips, blocks,
transactions, and tip-pinned address output scans. The exchange guide documents
deposit allocation, 100-confirmation starting policy, reorg handling,
withdrawal inspection and broadcast, backups, and incident recovery.

Website: https://btc09.org
Source: https://github.com/krutftw/bitcoin09
Release and checksums: https://github.com/krutftw/bitcoin09/releases/latest
Explorer: https://explorer.btc09.org
Integration guide: https://github.com/krutftw/bitcoin09/blob/master/docs/EXCHANGE-INTEGRATION.md
Discord: https://discord.gg/fUuGzwRTzP
Bitcointalk ANN: https://bitcointalk.org/index.php?topic=5587640.0

A SafeTrade market would give miners and new users a public transfer route
without relying entirely on informal Discord settlement. I'm available through
this ticket for integration testing and operational questions.

Thanks for reviewing it.

## One-time follow-up for ticket #39826

Hello,

Quick update on listing request #39826: v0.1.34 is live and per-block ASERT
activated successfully at height 12,096. The public exchange smoke test passes,
the network is progressing normally, and the integration guide is current:

https://github.com/krutftw/bitcoin09/blob/master/docs/EXCHANGE-INTEGRATION.md

Ticket #39837 appears to be a duplicate of this request, so please keep whichever
record you prefer. Is there anything else you need for the review?

Thanks.
