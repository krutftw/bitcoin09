package core

import "testing"

func TestBitsOnBranchDoesNotMaterializeEveryAncestor(t *testing.T) {
	params := MainNet
	chain := &Chain{
		params: &params,
		index:  make(map[Hash32]*blockIndex),
	}

	var parent *blockIndex
	for height := int64(0); height < 2048; height++ {
		header := Header{
			Version: 1,
			Bits:    params.MaxTargetBits,
			Time:    height + 1,
			Nonce:   uint64(height),
		}
		if parent != nil {
			header.PrevBlock = parent.id
		}
		block := &Block{Header: header}
		index := &blockIndex{
			block:  block,
			height: height,
			id:     header.ID(),
		}
		chain.index[index.id] = index
		parent = index
	}

	allocations := testing.AllocsPerRun(5, func() {
		if got := chain.bitsOnBranch(parent, parent.height+1); got != params.MaxTargetBits {
			t.Fatalf("bits = %08x, want %08x", got, params.MaxTargetBits)
		}
	})
	if allocations > 20 {
		t.Fatalf("non-retarget difficulty lookup allocated %.0f objects, want at most 20", allocations)
	}
}
