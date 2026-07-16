package core

import (
	"math/big"
	"testing"
	"time"
)

func TestMainNetASERTParameters(t *testing.T) {
	if MainNet.ASERTActivationHeight != 12096 || MainNet.ASERTHalfLife != 7200 ||
		MainNet.ASERTFutureDrift != 1800 || MainNet.ASERTMedianTimeBlocks != 11 {
		t.Fatalf("mainnet ASERT parameters = activation %d half-life %d drift %d median %d",
			MainNet.ASERTActivationHeight, MainNet.ASERTHalfLife,
			MainNet.ASERTFutureDrift, MainNet.ASERTMedianTimeBlocks)
	}
	if RegTest.ASERTActivationHeight != 0 || RegTest.ASERTHalfLife != 0 ||
		RegTest.ASERTFutureDrift != 0 || RegTest.ASERTMedianTimeBlocks != 0 {
		t.Fatalf("canonical regtest unexpectedly enables ASERT: %+v", RegTest)
	}
}

func TestCalculateASERTMatchesPublishedRun09(t *testing.T) {
	const (
		anchorHeight     = int64(2147483642)
		anchorParentTime = int64(1234567290)
		anchorBits       = uint32(0x1802aee8)
	)
	vectors := []struct {
		height int64
		time   int64
		bits   uint32
	}{
		{2147483643, 1234568190, 0x1802ae16},
		{2147483644, 1234568490, 0x1802ad44},
		{2147483645, 1234568790, 0x1802ac71},
		{2147483646, 1234569090, 0x1802ab9e},
		{2147483647, 1234569390, 0x1802aacd},
		{2147483648, 1234569690, 0x1802a9fa},
		{2147483649, 1234569990, 0x1802a929},
		{2147483650, 1234570290, 0x1802a858},
		{2147483651, 1234570590, 0x1802a787},
		{2147483652, 1234570890, 0x1802a6b7},
	}
	maxTarget := CompactToTarget(0x1d00ffff)
	for _, vector := range vectors {
		got := calculateASERTTarget(
			CompactToTarget(anchorBits),
			600,
			vector.time-anchorParentTime,
			vector.height-anchorHeight,
			172800,
			maxTarget,
		)
		if bits := TargetToCompact(got); bits != vector.bits {
			t.Fatalf("height %d ASERT bits = %08x, want %08x", vector.height, bits, vector.bits)
		}
	}
}

func TestCalculateASERTKeepsSteadySchedule(t *testing.T) {
	ref := CompactToTarget(0x1a2b3c4d)
	got := calculateASERTTarget(ref, 600, 6600, 10, 172800, CompactToTarget(0x1d00ffff))
	if got.Cmp(ref) != 0 {
		t.Fatalf("steady ASERT target = %x, want %x", got, ref)
	}
}

func TestCalculateASERTHalfLifeStepsAreExact(t *testing.T) {
	ref := CompactToTarget(0x1802aee8)
	maxTarget := CompactToTarget(0x1f00ffff)
	for _, test := range []struct {
		name     string
		timeDiff int64
		want     *big.Int
	}{
		{name: "one half-life behind", timeDiff: 600 + 172800, want: new(big.Int).Lsh(new(big.Int).Set(ref), 1)},
		{name: "one half-life ahead", timeDiff: 600 - 172800, want: new(big.Int).Rsh(new(big.Int).Set(ref), 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := calculateASERTTarget(ref, 600, test.timeDiff, 0, 172800, maxTarget)
			if got.Cmp(test.want) != 0 {
				t.Fatalf("ASERT target = %x, want %x", got, test.want)
			}
		})
	}
}

func TestCalculateASERTClampsTargetRange(t *testing.T) {
	maxTarget := CompactToTarget(0x1d00ffff)
	if got := calculateASERTTarget(maxTarget, 600, 1728000, 0, 172800, maxTarget); got.Cmp(maxTarget) != 0 {
		t.Fatalf("easy ASERT target = %x, want pow limit %x", got, maxTarget)
	}
	if got := calculateASERTTarget(big.NewInt(1), 600, 0, 1_000_000, 172800, maxTarget); got.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("hard ASERT target = %x, want 1", got)
	}
}

func TestNextBitsActivatesASERTAtConfiguredHeight(t *testing.T) {
	params := RegTest
	params.TargetBlockTime = 600
	params.RetargetInterval = 8
	params.MaxTargetBits = 0x1d00ffff
	params.ASERTActivationHeight = 8
	params.ASERTHalfLife = 7200
	params.ASERTFutureDrift = 1800
	params.ASERTMedianTimeBlocks = 3

	bits := []uint32{
		0x1c2b3c4d, 0x1c2b3c4d, 0x1c2b3c4d, 0x1c2b3c4d,
		0x1c2b3c4d, 0x1c2b3c4d, 0x1c2b3c4d, 0x1c2b3c4d,
	}
	times := []int64{0, 600, 1200, 1800, 2400, 3000, 3600, 3900}
	bitsAt := func(height int64) uint32 { return bits[height] }
	timeAt := func(height int64) int64 { return times[height] }

	if got := NextBits(&params, 7, bitsAt, timeAt); got != bits[6] {
		t.Fatalf("pre-activation bits = %08x, want legacy %08x", got, bits[6])
	}
	want := TargetToCompact(calculateASERTTarget(
		CompactToTarget(bits[7]), 600, times[7]-times[6], 0, 7200, params.MaxTarget(),
	))
	got := NextBits(&params, 8, bitsAt, timeAt)
	if got != want {
		t.Fatalf("activation bits = %08x, want ASERT %08x", got, want)
	}

	legacy := params
	legacy.ASERTActivationHeight = 0
	legacy.ASERTHalfLife = 0
	legacy.ASERTFutureDrift = 0
	legacy.ASERTMedianTimeBlocks = 0
	if old := NextBits(&legacy, 8, bitsAt, timeAt); old == got {
		t.Fatalf("activation accidentally matched legacy retarget: %08x", old)
	}
}

func TestASERTBranchUsesItsOwnAnchor(t *testing.T) {
	params := RegTest
	params.TargetBlockTime = 600
	params.ASERTActivationHeight = 4
	params.ASERTHalfLife = 7200
	params.ASERTFutureDrift = 1800
	params.ASERTMedianTimeBlocks = 3
	chain := &Chain{params: &params, index: make(map[Hash32]*blockIndex)}

	add := func(parent *blockIndex, height, timestamp int64, bits uint32, nonce uint64) *blockIndex {
		header := Header{Version: 1, Time: timestamp, Bits: bits, Nonce: nonce}
		if parent != nil {
			header.PrevBlock = parent.id
		}
		block := &Block{Header: header}
		index := &blockIndex{block: block, height: height, id: header.ID()}
		chain.index[index.id] = index
		return index
	}

	main := add(nil, 0, 0, params.MaxTargetBits, 0)
	chain.mainIDs = append(chain.mainIDs, main.id)
	for height := int64(1); height <= 6; height++ {
		main = add(main, height, height*600, params.MaxTargetBits, uint64(height))
		chain.mainIDs = append(chain.mainIDs, main.id)
	}
	chain.tip = main

	fork := chain.index[chain.mainIDs[2]]
	for height := int64(3); height <= 5; height++ {
		fork = add(fork, height, 1200+(height-2)*300, 0x2070ffff, uint64(100+height))
	}
	// The anchor is side-branch height 3, whose parent (height 2) is at 1200;
	// the current side parent is height 5 at 2100.
	want := TargetToCompact(calculateASERTTarget(
		CompactToTarget(0x2070ffff), 600, 2100-1200, 2, 7200, params.MaxTarget(),
	))
	if got := chain.bitsOnBranch(fork, 6); got != want {
		t.Fatalf("side-branch ASERT bits = %08x, want %08x", got, want)
	}
}

func TestPostASERTTimestampRules(t *testing.T) {
	params := RegTest
	params.ASERTActivationHeight = 12
	params.ASERTHalfLife = 7200
	params.ASERTFutureDrift = 1800
	params.ASERTMedianTimeBlocks = 11
	chain := &Chain{params: &params, index: make(map[Hash32]*blockIndex)}
	var parent *blockIndex
	for height := int64(0); height <= 11; height++ {
		header := Header{Version: 1, Time: 1000 + height*600, Bits: params.MaxTargetBits, Nonce: uint64(height)}
		if parent != nil {
			header.PrevBlock = parent.id
		}
		block := &Block{Header: header}
		parent = &blockIndex{block: block, height: height, id: header.ID()}
		chain.index[parent.id] = parent
	}
	now := time.Unix(10000, 0)
	if err := chain.checkBlockTimestamp(parent, 12, 4600, now); err == nil {
		t.Fatal("timestamp equal to MTP-11 was accepted")
	}
	if err := chain.checkBlockTimestamp(parent, 12, 4601, now); err != nil {
		t.Fatalf("timestamp one second after MTP-11 rejected: %v", err)
	}
	if err := chain.checkBlockTimestamp(parent, 12, 11800, now); err != nil {
		t.Fatalf("timestamp at ASERT future boundary rejected: %v", err)
	}
	if err := chain.checkBlockTimestamp(parent, 12, 11801, now); err == nil {
		t.Fatal("timestamp beyond ASERT future boundary was accepted")
	}
}

func TestLegacyTimestampRuleRemainsBeforeASERT(t *testing.T) {
	params := RegTest
	params.ASERTActivationHeight = 12
	params.ASERTHalfLife = 7200
	params.ASERTFutureDrift = 1800
	params.ASERTMedianTimeBlocks = 11
	parent := &blockIndex{height: 10, block: &Block{Header: Header{Time: 10000}}}
	chain := &Chain{params: &params}
	now := time.Unix(20000, 0)
	if err := chain.checkBlockTimestamp(parent, 11, 10000-params.FutureDrift, now); err != nil {
		t.Fatalf("legacy lower timestamp boundary rejected: %v", err)
	}
	if err := chain.checkBlockTimestamp(parent, 11, 9999-params.FutureDrift, now); err == nil {
		t.Fatal("timestamp below legacy lower boundary was accepted")
	}
}

func TestNewChainRejectsIncompleteASERTParameters(t *testing.T) {
	for _, mutate := range []func(*Params){
		func(p *Params) { p.ASERTActivationHeight = 1 },
		func(p *Params) { p.ASERTActivationHeight, p.ASERTHalfLife = 10, 0 },
		func(p *Params) { p.ASERTActivationHeight, p.ASERTHalfLife, p.ASERTFutureDrift = 10, 20, 0 },
		func(p *Params) {
			p.ASERTActivationHeight, p.ASERTHalfLife, p.ASERTFutureDrift, p.ASERTMedianTimeBlocks = 10, 20, 30, 2
		},
	} {
		params := RegTest
		mutate(&params)
		if _, err := NewChain(&params); err == nil {
			t.Fatalf("NewChain accepted incomplete ASERT parameters: %+v", params)
		}
	}
}

func TestASERTActivationRejectsLegacyRetargetBits(t *testing.T) {
	params := RegTest
	params.ASERTActivationHeight = 8
	params.ASERTHalfLife = 20
	params.ASERTFutureDrift = 3600
	params.ASERTMedianTimeBlocks = 3
	chain, err := NewChain(&params)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	_, pkh := keyAndPKH(t)
	base := time.Now().Unix() - 10
	for height := int64(1); height < params.ASERTActivationHeight; height++ {
		mineOneAt(t, chain, pkh, base)
	}

	legacy := params
	legacy.ASERTActivationHeight = 0
	legacy.ASERTHalfLife = 0
	legacy.ASERTFutureDrift = 0
	legacy.ASERTMedianTimeBlocks = 0
	bitsAt := func(height int64) uint32 { return chain.BlockAt(height).Header.Bits }
	timeAt := func(height int64) int64 { return chain.BlockAt(height).Header.Time }
	legacyBits := NextBits(&legacy, params.ASERTActivationHeight, bitsAt, timeAt)
	if legacyBits == chain.NextBitsForTip() {
		t.Fatalf("test setup produced identical ASERT and legacy bits %08x", legacyBits)
	}

	legacyCandidate := BuildBlockTemplate(chain, pkh, "legacy-retarget")
	legacyCandidate.Header.Time = base + 1
	legacyCandidate.Header.Bits = legacyBits
	if err := chain.AcceptBlock(legacyCandidate); err == nil {
		t.Fatal("legacy-retarget block was accepted at ASERT activation")
	}
	if block := mineOneAt(t, chain, pkh, base+1); block.Header.Bits == legacyBits {
		t.Fatalf("accepted activation block retained legacy bits %08x", legacyBits)
	}
}
