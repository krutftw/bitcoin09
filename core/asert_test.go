package core

import (
	"math/big"
	"testing"
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
