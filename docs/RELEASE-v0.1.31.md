# Bitcoin 09 v0.1.31

This release fixes a node performance problem that became visible when the
network started finding blocks much faster than usual.

## Node and network health

- Chain reload and snapshot validation no longer rebuild the full ancestor map
  for every block. Nodes with an existing chain now start and validate it much
  faster.
- The explorer reports the highest tip height advertised by a connected peer
  and shows when the local public node is behind. Peer heights are treated as
  untrusted health signals and never as consensus input.
- Mining concentration stats now separate single-output solo blocks from
  distributed or multi-output blocks such as PPLNS payouts.
- The website and Discord stats card use the new network-health fields while
  remaining compatible with an older explorer during deployment.

## Compatibility

There are no consensus, proof-of-work, wallet-format, P2P protocol, supply, or
mining-rule changes in this release. Valid blocks are still accepted under the
same rules, and existing nodes and wallets remain compatible.
