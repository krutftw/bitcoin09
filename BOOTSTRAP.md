# Bootstrap plan

Bitcoin did not start with exchanges, pools, explorers or companies. It
started with a client, one reachable node, a forum post, and people leaving
their computers on.

09C should start the same way.

## What exists now

- source code is public
- v0.1.18 binaries are published
- website is online at `https://btc09.org`
- mainnet genesis is fixed
- bootstrap seeds are online at `seed.btc09.org:9009`, `178.128.105.41:9009`, `103.80.18.140:9009`, and `108.190.240.138:9009`
- explorer is online at `https://explorer.btc09.org`
- Discord is live at `https://discord.gg/fUuGzwRTzP`
- OTC board is online at `https://btc09.org/markets.html`
- CPU mining works
- blocks and transactions sync between peers
- launch thread exists in GitHub issues
- GitHub discussions are open for network status and block reports

## What to do first

1. Keep the bootstrap seed nodes online.
2. Keep at least one home miner online.
3. Post the launch text from `POSTS.md`.
4. Ask early miners to reply with:
   - OS
   - CPU
   - whether they found a block
   - their node address if they can accept inbound connections
5. Add any reliable public peers to the README.
6. Keep telling early users to upgrade to the latest release if they mined on
   pre-v0.1.6 builds.

## Early peer discovery

Bitcoin used IRC discovery early on. 09C does not need to copy IRC exactly,
but it should copy the behaviour: one public place where nodes can find each
other.

Use Discord for live coordination:

```text
https://discord.gg/fUuGzwRTzP
```

The GitHub launch thread can still be used for slower public notes:

```text
https://github.com/krutftw/bitcoin09/issues/1
```

Anyone running a public node can post:

```text
public node: IP:9009
```

Those addresses can then be passed to `-seeds`:

```bash
btc09 node -mine -seeds seed.btc09.org:9009,178.128.105.41:9009,103.80.18.140:9009,108.190.240.138:9009
```

## When there are a few stable peers

Once the extra public nodes stay stable:

1. Register a domain.
2. Point `seed.btc09.org` at stable seed hosts.
3. Add DNS seed support to the client.
4. Keep the raw IP seeds as fallback.

## What not to add yet

- no exchange push
- no token wrapping
- no premine
- no pool as the main launch path
- no paid influencers
- no price talk as the pitch

The first job is distribution through mining, not hype.
