# Bitcoin 09: mining like it's 2009

**Abstract.** Bitcoin proved that a fixed-supply currency can run with no
issuer and no server, secured by proof of work. But its mining long ago
stopped being something a person does on their computer. Purpose-built
SHA-256 hardware pushed the cost of one lottery ticket from "leave your PC
on" to "buy a machine from a factory in Shenzhen". Bitcoin 09 re-runs the
original experiment with one change: a memory-hard proof of work that keeps
ordinary CPUs competitive. Every other rule is Bitcoin's.

## 1. Motivation

In 2009 anyone could download the client, leave it running, and win 50 coins
a few times a week. That open door is what built the early community, spread
the coins widely, and gave thousands of strangers a reason to care whether
the network lived. The door closed in stages: GPUs (2010), FPGAs (2011),
ASICs (2013). Today the network is safer than ever, but a newcomer with a
laptop owns a smaller share of it than a grain of sand owns of a beach.

The interesting question isn't whether Bitcoin can be improved. It probably
can't be, not in the ways that matter. The question is whether the 2009
experience can be run again, honestly: worthless coins, open mining, no
insiders, and let people decide what it becomes.

## 2. What changes, what doesn't

One thing changes: the proof of work function.

| Rule | Bitcoin | Bitcoin 09 |
|---|---|---|
| Proof of work | double SHA-256 | Argon2id, 64 MiB per attempt |
| Supply cap | 21,000,000 | same |
| Initial reward | 50 | same |
| Halving interval | 210,000 blocks | same |
| Block target | 10 minutes | same |
| Model | UTXO | same |
| Genesis reward | unspendable | same |
| Premine | none | none |

Signatures are Ed25519 rather than ECDSA because it is smaller, faster and
harder to misuse, and there is no legacy to stay compatible with.

## 3. Why memory-hard proof of work

SHA-256 is pure logic: the cheapest way to compute it is a chip that does
nothing else, which is why ASICs won. Argon2id is different. Each hash
attempt requires filling and randomly walking 64 MiB of memory. The cost of
an attempt is dominated by memory bandwidth, not logic. A GPU has thousands
of cores but they starve waiting on memory. An ASIC for Argon2id is mostly
a DRAM array, and DRAM is the one component where custom hardware buys
almost nothing over the sticks in a desktop.

Monero has run this argument in production for years with RandomX. CPU
mining there remains viable seven years on. Nothing is ASIC-proof forever,
but memory-hard functions keep the gap between a laptop and custom hardware
small enough that joining from home stays rational.

Block identifiers remain double SHA-256 of the header, so verifying a
chain's connectivity stays cheap. The Argon2id check runs once per block on
validation, which a Raspberry Pi handles easily.

## 4. Difficulty and emission

Difficulty retargets every 144 blocks (about one day at target rate) by the
ratio of actual to expected time, clamped to 4x per step. The launch
difficulty is set so a single desktop CPU finds a block in roughly ten
minutes, the same position a 2009 CPU had. As miners join, difficulty rises
and the raffle spreads out; the emission curve is Bitcoin's, so roughly half
the supply exists after four years and 99% after about 26 years.

## 5. Launch

The genesis block was mined on 6 July 2026, nonce 20214, id
ba685f741a04ddad03d37500ff354ce3887e64dd9cb6154ae236952792e90c3f. Its
coinbase message is "the coin that you can mine like it's 2009" and its
50-coin reward is paid to the all-zero key hash, which no key can produce.
Nobody, including the author, holds a coin that wasn't mined publicly at
block 1 or later. The code was public on GitHub before the first block.

## 6. What this is not

BTC09 has no company, no foundation, no allocation, no roadmap promises and
no price. It is worth nothing today and may be worth nothing forever. It
should be mined for the same reason Bitcoin was mined in 2009: because the
experiment is interesting and being early to it costs almost nothing. If a
market ever prices it, that will be the doing of people who chose it, not of
anyone who promised them anything.
