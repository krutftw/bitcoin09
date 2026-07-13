package core

import "testing"

func TestMerkleBranchReconstructsCanonicalRoot(t *testing.T) {
	transactions := make([]*Tx, 5)
	for index := range transactions {
		transactions[index] = NewCoinbase(int64(index+1), int64(index+1), [20]byte{byte(index + 1)}, "merkle-proof")
	}
	want := MerkleRoot(transactions)
	for index, transaction := range transactions {
		branch, err := MerkleBranch(transactions, index)
		if err != nil {
			t.Fatalf("index %d: %v", index, err)
		}
		got, err := MerkleRootFromBranch(transaction.ID(), index, branch)
		if err != nil {
			t.Fatalf("index %d: %v", index, err)
		}
		if got != want {
			t.Fatalf("index %d root = %x, want %x", index, got, want)
		}
	}
}

func TestMerkleBranchRejectsInvalidInput(t *testing.T) {
	transaction := NewCoinbase(1, 1, [20]byte{1}, "merkle-proof")
	if _, err := MerkleBranch(nil, 0); err == nil {
		t.Fatal("empty tree was accepted")
	}
	if _, err := MerkleBranch([]*Tx{transaction}, -1); err == nil {
		t.Fatal("negative index was accepted")
	}
	if _, err := MerkleBranch([]*Tx{transaction}, 1); err == nil {
		t.Fatal("out-of-range index was accepted")
	}
	if _, err := MerkleBranch([]*Tx{nil}, 0); err == nil {
		t.Fatal("nil transaction was accepted")
	}
	if _, err := MerkleRootFromBranch(transaction.ID(), 2, []Hash32{{1}}); err == nil {
		t.Fatal("index outside branch depth was accepted")
	}
}
