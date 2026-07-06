// Package core implements the Bitcoin 09 (09C) consensus rules.
//
// Bitcoin 09 is Bitcoin, same-same but different in exactly one area:
// proof-of-work is Argon2id (memory-hard) instead of SHA-256d, so ordinary
// CPUs mine it and ASICs/GPUs gain almost nothing. Everything economic is
// Bitcoin-classic: 21M cap, 50-coin subsidy, halving every 210,000 blocks,
// 10-minute blocks, fair launch, no premine, unspendable genesis reward.
package core

import "math/big"

const (
	CoinName     = "Bitcoin 09"
	Ticker       = "09C"
	UnitsPerCoin = 100_000_000 // smallest unit, like satoshis

	InitialRewardUnits = 50 * UnitsPerCoin
	HalvingInterval    = 210_000 // blocks, Bitcoin-classic
	MaxHalvings        = 64

	MaxBlockBytes = 1_000_000 // 1 MB, Bitcoin-classic
)

// Params defines a network (mainnet or regtest for local testing).
type Params struct {
	Name             string
	NetMagic         uint32 // wire/network id
	TargetBlockTime  int64  // seconds
	RetargetInterval int64  // blocks between difficulty adjustments
	CoinbaseMaturity int64  // blocks before a coinbase output is spendable
	MaxTargetBits    uint32 // easiest allowed difficulty, compact encoding
	// Argon2id proof-of-work cost parameters (per hash attempt).
	ArgonMemKiB uint32
	ArgonTime   uint32
	// Genesis
	GenesisTime     int64
	GenesisNonce    uint64
	GenesisHeadline string
	FutureDrift     int64 // max seconds a block time may be ahead of local clock
}

var MainNet = Params{
	Name:             "mainnet",
	NetMagic:         0xB7C09001,
	TargetBlockTime:  600, // 10 minutes, Bitcoin-classic
	RetargetInterval: 2016, // Bitcoin-classic, ~2 weeks at target rate
	CoinbaseMaturity: 100, // Bitcoin-classic
	MaxTargetBits:    0x1f00ffff,
	ArgonMemKiB:      64 * 1024, // 64 MiB per hash: the ASIC/GPU wall
	ArgonTime:        1,
	GenesisTime:      1783274400, // 2026-07-05 18:00:00 UTC, launch day
	GenesisNonce:     20214,      // mined 2026-07-06; genesis id ba685f741a04ddad03d37500ff354ce3887e64dd9cb6154ae236952792e90c3f
	GenesisHeadline:  "the coin that you can mine like it's 2009",
	FutureDrift:      2 * 3600,
}

var RegTest = Params{
	Name:             "regtest",
	NetMagic:         0xB7C09BAD,
	TargetBlockTime:  1,
	RetargetInterval: 8,
	CoinbaseMaturity: 2,
	MaxTargetBits:    0x207fffff, // ~1 in 2 hashes wins
	ArgonMemKiB:      1024,       // 1 MiB, fast for tests
	ArgonTime:        1,
	GenesisTime:      1751738400,
	GenesisNonce:     0,
	GenesisHeadline:  "regtest genesis",
	FutureDrift:      2 * 3600,
}

// MaxTarget returns the easiest-allowed PoW target for the network.
func (p *Params) MaxTarget() *big.Int { return CompactToTarget(p.MaxTargetBits) }

// SubsidyAt returns the block subsidy in units at a given height.
// 50 coins, halving every 210,000 blocks, exactly like Bitcoin.
func SubsidyAt(height int64) int64 {
	halvings := height / HalvingInterval
	if halvings >= MaxHalvings {
		return 0
	}
	return InitialRewardUnits >> uint(halvings)
}
