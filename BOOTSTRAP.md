# Bootstrap plan

Bitcoin did not start with exchanges, pools, explorers or companies. It
started with a client, one reachable node, a forum post, and people leaving
their computers on.

BTC09 should start the same way.

## What exists now

- source code is public
- v0.1.0 binaries are published
- mainnet genesis is fixed
- seed node is online at `82.22.32.82:9009`
- CPU mining works
- blocks and transactions sync between peers
- launch thread exists in GitHub issues

## What to do first

1. Keep the seed node online.
2. Keep at least one home miner online.
3. Post the launch text from `POSTS.md`.
4. Ask early miners to reply with:
   - OS
   - CPU
   - whether they found a block
   - their node address if they can accept inbound connections
5. Add any reliable public peers to the README.

## Early peer discovery

Bitcoin used IRC discovery early on. BTC09 does not need to copy IRC exactly,
but it should copy the behaviour: one public place where nodes can find each
other.

Use the GitHub launch thread for now:

```text
https://github.com/krutftw/bitcoin09/issues/1
```

Anyone running a public node can post:

```text
public node: IP:9009
```

Those addresses can then be passed to `-seeds`:

```bash
btc09 node -mine -seeds 82.22.32.82:9009,OTHER_IP:9009
```

## When there are a few stable peers

Once at least three independent public nodes are online:

1. Add them to README as bootstrap peers.
2. Register a domain.
3. Point `seed.bitcoin09.org` to the first stable seed.
4. Add DNS seed support to the client.
5. Keep the raw IP seed as fallback.

## What not to add yet

- no exchange push
- no token wrapping
- no premine
- no pool as the main launch path
- no paid influencers
- no price talk as the pitch

The first job is distribution through mining, not hype.
