package p2p

import "testing"

func TestHighestAdvertisedHeightTracksConnectedPeersMonotonically(t *testing.T) {
	first := &peer{}
	second := &peer{}
	node := &Node{peers: map[string]*peer{
		"first":  first,
		"second": second,
	}}

	node.notePeerHeight(first, 120)
	node.notePeerHeight(second, 140)
	if got := node.HighestAdvertisedHeight(); got != 140 {
		t.Fatalf("highest advertised height = %d, want 140", got)
	}

	node.notePeerHeight(second, 130)
	if got := node.HighestAdvertisedHeight(); got != 140 {
		t.Fatalf("advertised height regressed to %d, want 140", got)
	}

	node.mu.Lock()
	delete(node.peers, "second")
	node.mu.Unlock()
	if got := node.HighestAdvertisedHeight(); got != 120 {
		t.Fatalf("highest advertised height after disconnect = %d, want 120", got)
	}
}
