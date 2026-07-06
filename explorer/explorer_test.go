package explorer

import (
	"testing"

	"github.com/krutftw/bitcoin09/core"
)

func TestSupplyExcludesBurnedGenesisReward(t *testing.T) {
	got := supplyAt(&core.MainNet, 0)
	if got.CirculatingSupplyUnits != 0 {
		t.Fatalf("height 0 circulating supply = %d, want 0", got.CirculatingSupplyUnits)
	}
	if got.TotalSubsidyIssuedUnits != core.InitialRewardUnits {
		t.Fatalf("height 0 issued supply = %d, want genesis subsidy", got.TotalSubsidyIssuedUnits)
	}

	got = supplyAt(&core.MainNet, 1)
	if got.CirculatingSupplyUnits != core.InitialRewardUnits {
		t.Fatalf("height 1 circulating supply = %d, want one spendable-era subsidy", got.CirculatingSupplyUnits)
	}
}

func TestSupplyHalvingBoundaries(t *testing.T) {
	before := supplyAt(&core.MainNet, core.HalvingInterval-1)
	if before.BlockRewardUnits != core.InitialRewardUnits/2 {
		t.Fatalf("next halving block reward = %d, want first halved subsidy", before.BlockRewardUnits)
	}
	if before.NextHalvingHeight != core.HalvingInterval || before.BlocksToHalving != 1 {
		t.Fatalf("next halving = height %d in %d blocks, want height %d in 1 block",
			before.NextHalvingHeight, before.BlocksToHalving, core.HalvingInterval)
	}

	after := supplyAt(&core.MainNet, core.HalvingInterval)
	if after.BlockRewardUnits != core.InitialRewardUnits/2 {
		t.Fatalf("post-halving next reward = %d, want %d", after.BlockRewardUnits, core.InitialRewardUnits/2)
	}
	if after.NextHalvingHeight != 2*core.HalvingInterval {
		t.Fatalf("post-halving next height = %d, want %d", after.NextHalvingHeight, 2*core.HalvingInterval)
	}
}

func TestSupplyCapAndZeroSubsidy(t *testing.T) {
	got := supplyAt(&core.MainNet, 2_639)
	want := int64(2_639) * core.InitialRewardUnits
	if got.CirculatingSupplyUnits != want {
		t.Fatalf("circulating at 2639 = %d, want %d", got.CirculatingSupplyUnits, want)
	}
	if got.ZeroSubsidyHeight != 6_930_000 {
		t.Fatalf("zero subsidy height = %d, want 6930000", got.ZeroSubsidyHeight)
	}
	if got.MaximumCirculatingSupplyUnits >= got.MaxSupplyUnits {
		t.Fatalf("maximum circulating supply should stay below nominal cap because genesis is burned")
	}
}
