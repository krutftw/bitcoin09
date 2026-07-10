package p2p

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

func TestGetBlocksSendsBoundedBatches(t *testing.T) {
	c := newBatchTestChain(t)
	_, pkh := batchTestKey(t)
	for i := 0; i < blockBatchSize+2; i++ {
		mineBatchTestBlock(t, c, pkh)
	}

	n := NewNode(c, ":0", log.New(io.Discard, "", 0))

	firstBatch := getBlockBatch(t, n, 1)
	if len(firstBatch) != blockBatchSize {
		t.Fatalf("first batch length = %d, want %d", len(firstBatch), blockBatchSize)
	}
	for i, msg := range firstBatch {
		wantHeight := int64(i + 1)
		if msg.Type != "block" || msg.Height != wantHeight {
			t.Fatalf("message %d = type %q height %d, want block %d", i, msg.Type, msg.Height, wantHeight)
		}
		if i < len(firstBatch)-1 && msg.More {
			t.Fatalf("message %d has More=true before final batch item", i)
		}
	}
	if !firstBatch[len(firstBatch)-1].More {
		t.Fatal("first batch final message should advertise more blocks")
	}

	finalBatch := getBlockBatch(t, n, 3)
	if len(finalBatch) != blockBatchSize {
		t.Fatalf("final batch length = %d, want %d", len(finalBatch), blockBatchSize)
	}
	if finalBatch[len(finalBatch)-1].More {
		t.Fatal("final batch should not advertise more blocks at chain tip")
	}
}

func TestReserveDialCandidatesCapsOutboundHandshakes(t *testing.T) {
	c := newBatchTestChain(t)
	n := NewNode(c, ":0", log.New(io.Discard, "", 0))
	for i := 1; i <= maxPeers+20; i++ {
		n.known[fmt.Sprintf("203.0.113.%d:9009", i)] = true
	}

	first := n.reserveDialCandidates()
	if len(first) != 4 {
		t.Fatalf("first reservation = %d candidates, want 4", len(first))
	}
	if second := n.reserveDialCandidates(); len(second) != 0 {
		t.Fatalf("second reservation = %d candidates, want 0 while outbound cap is in flight", len(second))
	}
	if _, ok := n.reserveInboundHandshake(); !ok {
		t.Fatal("outbound reservations starved inbound handshake capacity")
	}
}

func TestReserveDialCandidatesPrioritizesConfiguredSeeds(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	seed := "seed.btc09.org:9009"
	n.known[seed] = true
	n.trustedSeeds[seed] = true
	n.seedTargets = append(n.seedTargets, seed)
	for i := 1; i <= 100; i++ {
		n.known[fmt.Sprintf("8.8.%d.%d:9009", i/255, i%255)] = true
	}

	reservations := n.reserveDialCandidates()
	if len(reservations) != maxPendingOutbound {
		t.Fatalf("outbound reservations = %d, want %d", len(reservations), maxPendingOutbound)
	}
	foundSeed := false
	for _, reservation := range reservations {
		if reservation.target == seed {
			foundSeed = true
			break
		}
	}
	if !foundSeed {
		t.Fatalf("configured seed was starved by gossip targets: %#v", reservations)
	}
}

func TestReserveDialCandidatesDoesNotStarveGossipBehindSeeds(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	for i := 1; i <= maxPendingOutbound; i++ {
		seed := fmt.Sprintf("seed-%d.btc09.test:9009", i)
		n.known[seed] = true
		n.trustedSeeds[seed] = true
		n.seedTargets = append(n.seedTargets, seed)
	}
	gossip := "8.8.8.8:9009"
	n.known[gossip] = true

	reservations := n.reserveDialCandidates()
	foundSeed := false
	foundGossip := false
	for _, reservation := range reservations {
		foundSeed = foundSeed || n.trustedSeeds[reservation.target]
		foundGossip = foundGossip || reservation.target == gossip
	}
	if !foundSeed || !foundGossip {
		t.Fatalf("mixed candidate batch must include seed and gossip: %#v", reservations)
	}
}

func TestReserveDialCandidatesRotatesMoreThanFourConfiguredSeeds(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	for i := 1; i <= maxPendingOutbound+2; i++ {
		seed := fmt.Sprintf("seed-%d.btc09.test:9009", i)
		n.known[seed] = true
		n.trustedSeeds[seed] = true
		n.seedTargets = append(n.seedTargets, seed)
	}

	first := n.reserveDialCandidates()
	firstTargets := make(map[string]bool, len(first))
	for _, reservation := range first {
		firstTargets[reservation.target] = true
		n.releaseHandshake(reservation)
	}
	second := n.reserveDialCandidates()
	foundNewSeed := false
	for _, reservation := range second {
		if !firstTargets[reservation.target] {
			foundNewSeed = true
			break
		}
	}
	if !foundNewSeed {
		t.Fatalf("configured-seed rotation retried only the first batch: first=%#v second=%#v", first, second)
	}
}

func TestReserveDialCandidatesRotatesGossipTargetsFairly(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	for i := 1; i <= maxPendingOutbound+2; i++ {
		n.known[fmt.Sprintf("8.8.8.%d:9009", i)] = true
	}

	first := n.reserveDialCandidates()
	firstTargets := make(map[string]bool, len(first))
	for _, reservation := range first {
		firstTargets[reservation.target] = true
		n.releaseHandshake(reservation)
	}
	second := n.reserveDialCandidates()
	foundNewTarget := false
	for _, reservation := range second {
		if !firstTargets[reservation.target] {
			foundNewTarget = true
			break
		}
	}
	if !foundNewTarget {
		t.Fatalf("gossip rotation retried only the first batch: first=%#v second=%#v", first, second)
	}
}

func TestHandshakeReservationCleanupIsOwnerChecked(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	target := "8.8.8.8:9009"
	first := reserveOutbound(t, n, target)
	n.releaseHandshake(first)
	second := reserveOutbound(t, n, target)

	n.releaseHandshake(first)
	n.mu.RLock()
	gotToken, stillDialing := n.dialing[target]
	_, stillPending := n.pending[second.token]
	n.mu.RUnlock()
	if !stillDialing || gotToken != second.token || !stillPending {
		t.Fatal("stale cleanup removed the newer handshake reservation")
	}
	n.releaseHandshake(second)
}

func TestInboundHandshakesAreCapped(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	reservations := make([]handshakeReservation, 0, maxPendingInbound)
	for i := 0; i < maxPendingInbound; i++ {
		reservation, ok := n.reserveInboundHandshake()
		if !ok {
			t.Fatalf("inbound reservation %d was rejected before maxPendingInbound", i)
		}
		reservations = append(reservations, reservation)
	}
	if _, ok := n.reserveInboundHandshake(); ok {
		t.Fatal("inbound reservation exceeded maxPendingInbound")
	}
	n.releaseHandshake(reservations[0])
	if replacement, ok := n.reserveInboundHandshake(); !ok {
		t.Fatal("released inbound slot was not reusable")
	} else {
		n.releaseHandshake(replacement)
	}
}

func TestPendingInboundHandshakesAreCappedPerSource(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	for i := 0; i < maxPendingInboundPerIP; i++ {
		if _, ok := n.reserveInboundHandshakeFrom(fmt.Sprintf("8.8.8.8:%d", 50000+i)); !ok {
			t.Fatalf("same-source inbound reservation %d rejected too early", i)
		}
	}
	if _, ok := n.reserveInboundHandshakeFrom("8.8.8.8:60000"); ok {
		t.Fatal("same source exceeded its pending inbound handshake limit")
	}
	if _, ok := n.reserveInboundHandshakeFrom("1.1.1.1:50000"); !ok {
		t.Fatal("one saturated source blocked a different source")
	}
}

func TestConnectedInboundPeersLeaveReservedOutboundSlots(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	for i := 0; i < maxPeers-reservedOutboundSlots; i++ {
		reservation, ok := n.reserveInboundHandshake()
		if !ok {
			t.Fatalf("inbound reservation %d was rejected", i)
		}
		key := fmt.Sprintf("legacy-conn:peer-%d", i)
		if !n.registerPeer(reservation, key, "", &peer{addr: key}) {
			t.Fatalf("inbound peer %d was rejected", i)
		}
	}
	if _, ok := n.reserveInboundHandshake(); ok {
		t.Fatal("connected inbound peers consumed an outbound-reserved slot")
	}

	target := "8.8.8.8:9009"
	n.mu.Lock()
	n.known[target] = true
	n.mu.Unlock()
	if got := n.reserveDialCandidates(); len(got) != 1 || got[0].target != target {
		t.Fatalf("outbound reservation with inbound quota full = %#v", got)
	}
}

func TestConnectedHostnameAndAdvertisedIPAreNotRedialed(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	reservation := reserveOutbound(t, n, "seed.btc09.org:9009")
	key := "node:" + strings.Repeat("c", 32)
	p := &peer{addr: key}
	if !n.registerPeer(reservation, key, "8.8.8.8:9009", p) {
		t.Fatal("peer registration failed")
	}
	if got := n.reserveDialCandidates(); len(got) != 0 {
		t.Fatalf("connected hostname/IP aliases were reserved again: %#v", got)
	}
}

func TestInboundAdvertisedEndpointIsNotRedialedWhileConnected(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	reservation, ok := n.reserveInboundHandshake()
	if !ok {
		t.Fatal("inbound reservation failed")
	}
	key := "node:" + strings.Repeat("d", 32)
	if !n.registerPeer(reservation, key, "1.1.1.1:9009", &peer{addr: key}) {
		t.Fatal("inbound peer registration failed")
	}
	if got := n.reserveDialCandidates(); len(got) != 0 {
		t.Fatalf("connected inbound endpoint was reserved for callback: %#v", got)
	}
}

func TestRegisterPeerRejectsDuplicateAndStaleCleanupCannotDeleteOwner(t *testing.T) {
	c := newBatchTestChain(t)
	n := NewNode(c, ":0", log.New(io.Discard, "", 0))
	key := "node:" + strings.Repeat("a", 32)
	primary := &peer{addr: key, remoteIP: "8.8.8.8", connectedAt: time.Now()}
	duplicate := &peer{addr: key, remoteIP: "8.8.8.8", connectedAt: time.Now()}
	primaryReservation := reserveOutbound(t, n, "seed.btc09.org:9009")

	if !n.registerPeer(primaryReservation, key, "", primary) {
		t.Fatal("first connection was rejected")
	}
	duplicateReservation := reserveOutbound(t, n, "8.8.8.8:9009")
	if n.registerPeer(duplicateReservation, key, "", duplicate) {
		t.Fatal("duplicate connection was accepted")
	}

	n.unregisterPeer(key, duplicate)
	if got := n.peers[key]; got != primary {
		t.Fatalf("stale cleanup removed or replaced the active peer: got %p want %p", got, primary)
	}
	if got := n.targets["8.8.8.8:9009"]; got != key {
		t.Fatalf("duplicate dial target maps to %q, want %q", got, key)
	}

	n.unregisterPeer(key, primary)
	if _, ok := n.peers[key]; ok {
		t.Fatal("active peer remained after owner cleanup")
	}
	if _, ok := n.targets["seed.btc09.org:9009"]; ok {
		t.Fatal("hostname target remained after peer cleanup")
	}
	if _, ok := n.targets["8.8.8.8:9009"]; ok {
		t.Fatal("IP target remained after peer cleanup")
	}
}

func TestRegisterPeerDeterministicallyReplacesConcurrentAliasWithHigherTieKey(t *testing.T) {
	c := newBatchTestChain(t)
	n := NewNode(c, ":0", log.New(io.Discard, "", 0))
	key := "node:" + strings.Repeat("b", 32)
	now := time.Now()
	higher := &peer{addr: key, tieKey: "z-connection", remoteIP: "8.8.8.8", connectedAt: now}
	lower := &peer{addr: key, tieKey: "a-connection", remoteIP: "8.8.8.8", connectedAt: now.Add(time.Millisecond)}
	higherReservation := reserveOutbound(t, n, "seed.btc09.org:9009")

	if !n.registerPeer(higherReservation, key, "", higher) {
		t.Fatal("first connection was rejected")
	}
	lowerReservation := reserveOutbound(t, n, "8.8.8.8:9009")
	if !n.registerPeer(lowerReservation, key, "", lower) {
		t.Fatal("deterministically preferred duplicate was rejected")
	}
	if got := n.peers[key]; got != lower {
		t.Fatalf("active peer = %p, want lower tie-key peer %p", got, lower)
	}

	n.unregisterPeer(key, higher)
	if got := n.peers[key]; got != lower {
		t.Fatalf("replaced peer cleanup removed new owner: got %p want %p", got, lower)
	}
}

func TestLaterDuplicateCannotEvictEstablishedNodeID(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	key := "node:" + strings.Repeat("f", 32)
	existing := &peer{
		addr: key, tieKey: "z-connection", remoteIP: "8.8.8.8",
		connectedAt: time.Now().Add(-duplicateCrossDialWindow - time.Second),
	}
	first := reserveOutbound(t, n, "seed.btc09.org:9009")
	if !n.registerPeer(first, key, "8.8.8.8:9009", existing) {
		t.Fatal("first peer was rejected")
	}

	candidate := &peer{
		addr: key, tieKey: "a-connection", remoteIP: "8.8.8.8",
		connectedAt: time.Now(),
	}
	second, ok := n.reserveInboundHandshake()
	if !ok {
		t.Fatal("duplicate reservation was rejected")
	}
	if n.registerPeer(second, key, "8.8.8.8:9009", candidate) {
		t.Fatal("later duplicate replaced an established claimed node ID")
	}
	if got := n.peers[key]; got != existing {
		t.Fatalf("established peer owner = %p, want %p", got, existing)
	}
}

func TestNormalizeGossipAddressRejectsPrivateAndHostnames(t *testing.T) {
	for _, addr := range []string{
		"8.8.8.8:9009",
		"[2001:4860:4860::8888]:9009",
	} {
		if got, ok := normalizeGossipAddress(addr); !ok || got != addr {
			t.Fatalf("normalizeGossipAddress(%q) = %q, %v; want unchanged, true", addr, got, ok)
		}
	}

	for _, addr := range []string{
		"127.0.0.1:9009",
		"0.1.2.3:9009",
		"10.0.0.1:9009",
		"169.254.1.2:9009",
		"100.64.0.1:9009",
		"192.0.0.1:9009",
		"192.0.2.1:9009",
		"198.18.0.1:9009",
		"198.51.100.1:9009",
		"203.0.113.1:9009",
		"240.0.0.1:9009",
		"[::1]:9009",
		"[64:ff9b::1]:9009",
		"[100::1]:9009",
		"[2001:100::1]:9009",
		"[2001:db8::1]:9009",
		"[2002::1]:9009",
		"[3ffe::1]:9009",
		"[3fff::1]:9009",
		"[4000::1]:9009",
		"[5f00::1]:9009",
		"[fec0::1]:9009",
		"[2001:4860:4860::8888%eth0]:9009",
		"seed.example:9009",
		"8.8.8.8:0",
		"8.8.8.8:70000",
		"8.8.8.8",
	} {
		if got, ok := normalizeGossipAddress(addr); ok {
			t.Fatalf("normalizeGossipAddress(%q) = %q, true; want rejection", addr, got)
		}
	}
}

func TestPeerIdentityUsesNodeIDAndSeparatesNATedNodes(t *testing.T) {
	idA := strings.Repeat("a", 32)
	idB := strings.Repeat("b", 32)
	if got := peerIdentity(idA, "8.8.8.8:54321", "8.8.8.8:9009"); got != "node:"+idA+"@8.8.8.8" {
		t.Fatalf("peerIdentity(node A) = %q", got)
	}
	if peerIdentity(idA, "8.8.8.8:54321", "8.8.8.8:9009") == peerIdentity(idB, "8.8.8.8:54322", "8.8.8.8:9009") {
		t.Fatal("distinct node IDs behind one public IP collapsed")
	}
	if peerIdentity(idA, "8.8.8.8:54321", "8.8.8.8:9009") == peerIdentity(idA, "1.1.1.1:54321", "1.1.1.1:9009") {
		t.Fatal("an unauthenticated node ID claim crossed observed source IPs")
	}
	if got := peerIdentity("", "8.8.8.8:54321", "8.8.8.8:9009"); got != "legacy:8.8.8.8:9009" {
		t.Fatalf("legacy peer identity = %q", got)
	}
	if peerIdentity("", "8.8.8.8:54321", "8.8.8.8:9009") != peerIdentity("", "8.8.8.8:54322", "8.8.8.8:9009") {
		t.Fatal("legacy aliases with one advertised endpoint did not deduplicate")
	}
	if got := peerIdentity("", "8.8.8.8:54321", ""); got != "legacy-conn:8.8.8.8:54321" {
		t.Fatalf("non-listening legacy identity = %q", got)
	}
}

func TestConnectionTieKeyIsDirectionIndependent(t *testing.T) {
	idA := strings.Repeat("a", 32)
	idB := strings.Repeat("b", 32)
	nonceA := strings.Repeat("1", 32)
	nonceB := strings.Repeat("2", 32)
	if got, want := connectionTieKey(idA, nonceA, idB, nonceB), connectionTieKey(idB, nonceB, idA, nonceA); got != want {
		t.Fatalf("tie keys differ by direction: %q != %q", got, want)
	}
}

func TestDialablePeerEndpointRequiresPublicHostAndListenPort(t *testing.T) {
	if got, ok := dialablePeerEndpoint("8.8.8.8:54321", ":9009"); !ok || got != "8.8.8.8:9009" {
		t.Fatalf("dialable endpoint = %q, %v", got, ok)
	}
	for _, listen := range []string{"127.0.0.1:0", ":0", "bad"} {
		if got, ok := dialablePeerEndpoint("8.8.8.8:54321", listen); ok {
			t.Fatalf("listen %q produced dialable endpoint %q", listen, got)
		}
	}
}

func TestAddrMessageOnlyAddsPublicIPTargets(t *testing.T) {
	c := newBatchTestChain(t)
	n := NewNode(c, ":0", log.New(io.Discard, "", 0))
	p := &peer{addr: "8.8.8.8:9009"}
	if err := n.handleMsg(p, &Msg{Type: "addr", Addrs: []string{
		"1.1.1.1:9009",
		"127.0.0.1:9009",
		"10.0.0.1:9009",
		"100.64.0.1:9009",
		"198.18.0.1:9009",
		"192.0.2.1:9009",
		"metadata.internal:80",
	}}); err != nil {
		t.Fatalf("handle addr: %v", err)
	}
	if !n.known["1.1.1.1:9009"] {
		t.Fatal("public peer address was not retained")
	}
	for _, rejected := range []string{"127.0.0.1:9009", "10.0.0.1:9009", "100.64.0.1:9009", "198.18.0.1:9009", "192.0.2.1:9009", "metadata.internal:80"} {
		if n.known[rejected] {
			t.Fatalf("unsafe gossip address %q was retained", rejected)
		}
	}
}

func TestAliasSeedsConvergeOnOnePeerWithoutActiveDialReservations(t *testing.T) {
	seedListener := testListener(t)
	clientListener := testListener(t)
	seedAddr := seedListener.Addr().String()
	clientAddr := clientListener.Addr().String()
	_, seedPort, err := net.SplitHostPort(seedAddr)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	seed := NewNode(newBatchTestChain(t), seedAddr, log.New(io.Discard, "", 0))
	if err := seed.startWithListener(ctx, seedListener, nil); err != nil {
		t.Fatalf("start seed: %v", err)
	}
	client := NewNode(newBatchTestChain(t), clientAddr, log.New(io.Discard, "", 0))
	if err := client.startWithListener(ctx, clientListener, []string{"localhost:" + seedPort, seedAddr}); err != nil {
		t.Fatalf("start client: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		client.mu.RLock()
		activeDials := len(client.dialing)
		client.mu.RUnlock()
		if client.PeerCount() == 1 && seed.PeerCount() == 1 && activeDials == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got := client.PeerCount(); got != 1 {
		t.Fatalf("client peer count = %d, want 1 for hostname/IP aliases", got)
	}
	if got := seed.PeerCount(); got != 1 {
		t.Fatalf("seed peer count = %d, want 1 for duplicate incoming identity", got)
	}

	client.mu.RLock()
	activeDials := len(client.dialing)
	client.mu.RUnlock()
	if activeDials != 0 {
		t.Fatalf("active dial reservations = %d after handshake, want 0", activeDials)
	}
	cancel()
	waitFor(t, 2*time.Second, func() bool { return client.PeerCount() == 0 && seed.PeerCount() == 0 })
}

func TestSimultaneousCrossDialConvergesOnSameConnection(t *testing.T) {
	listenerA := testListener(t)
	listenerB := testListener(t)
	addrA := listenerA.Addr().String()
	addrB := listenerB.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	nodeA := NewNode(newBatchTestChain(t), addrA, log.New(io.Discard, "", 0))
	nodeB := NewNode(newBatchTestChain(t), addrB, log.New(io.Discard, "", 0))
	if err := nodeA.startWithListener(ctx, listenerA, []string{addrB}); err != nil {
		t.Fatal(err)
	}
	if err := nodeB.startWithListener(ctx, listenerB, []string{addrA}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return nodeA.PeerCount() == 1 && nodeB.PeerCount() == 1 })
	time.Sleep(200 * time.Millisecond)
	if nodeA.PeerCount() != 1 || nodeB.PeerCount() != 1 {
		t.Fatalf("cross dial did not stabilize: A=%d B=%d", nodeA.PeerCount(), nodeB.PeerCount())
	}
	cancel()
	waitFor(t, 2*time.Second, func() bool { return nodeA.PeerCount() == 0 && nodeB.PeerCount() == 0 })
}

func TestHandshakeDeadlineClearedBeforePeerPublication(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	reservation, ok := n.reserveInboundHandshake()
	if !ok {
		t.Fatal("could not reserve inbound handshake")
	}

	serverRaw, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer client.Close()

	peerCountAtClear := make(chan int, 1)
	server := &deadlineObservingConn{
		Conn: serverRaw,
		onClear: func() {
			select {
			case peerCountAtClear <- n.PeerCount():
			default:
			}
		},
	}

	remoteDone := make(chan struct{})
	go func() {
		defer close(remoteDone)
		r := bufio.NewReader(client)
		if _, err := readMsg(r); err != nil {
			return
		}
		remote := &peer{conn: client, enc: bufio.NewWriter(client)}
		_ = remote.sendWithTimeout(&Msg{
			Type: "hello", Version: protocolVersion, Magic: n.chain.Params().NetMagic,
			NodeID: strings.Repeat("a", 32), Nonce: strings.Repeat("b", 32),
		}, time.Second)
		<-ctx.Done()
	}()

	handlerDone := make(chan struct{})
	go func() {
		n.handleConn(ctx, server, reservation)
		close(handlerDone)
	}()

	select {
	case got := <-peerCountAtClear:
		if got != 0 {
			t.Fatalf("peer count when handshake deadline cleared = %d, want 0", got)
		}
	case <-time.After(time.Second):
		t.Fatal("handshake deadline was not cleared")
	}
	cancel()
	client.Close()
	<-handlerDone
	<-remoteDone
}

func TestSelfSeedIsPersistentlySuppressed(t *testing.T) {
	listener := testListener(t)
	addr := listener.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := NewNode(newBatchTestChain(t), addr, log.New(io.Discard, "", 0))
	if err := n.startWithListener(ctx, listener, []string{addr}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool {
		n.mu.RLock()
		defer n.mu.RUnlock()
		return len(n.pending) == 0
	})

	n.mu.RLock()
	known := n.known[addr]
	n.mu.RUnlock()
	if known {
		t.Fatal("self seed remained dialable after node-ID self-detection")
	}
	if got := n.reserveDialCandidates(); len(got) != 0 {
		t.Fatalf("self seed was reserved again: %#v", got)
	}
}

func TestSuppressedSelfTargetCannotBeReintroducedByGossip(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	target := "8.8.8.8:9009"
	n.suppressSelfTargets(target)
	if err := n.handleMsg(&peer{}, &Msg{Type: "addr", Addrs: []string{target}}); err != nil {
		t.Fatal(err)
	}
	n.mu.RLock()
	known := n.known[target]
	n.mu.RUnlock()
	if known {
		t.Fatal("gossip reintroduced a target already proven to be self")
	}
}

func TestInboundSaturationPreservesOutboundCapacity(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	accepted := 0
	for i := 0; i < maxPeers; i++ {
		if _, ok := n.reserveInboundHandshake(); ok {
			accepted++
		}
	}
	if accepted == maxPeers {
		t.Fatalf("inbound handshakes consumed all %d peer slots", maxPeers)
	}

	target := "8.8.8.8:9009"
	n.mu.Lock()
	n.known[target] = true
	n.mu.Unlock()
	reservations := n.reserveDialCandidates()
	if len(reservations) != 1 || reservations[0].target != target {
		t.Fatalf("outbound seed reservation after inbound saturation = %#v", reservations)
	}
}

func TestDifferentSourceCannotReplaceClaimedNodeID(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	key := "node:" + strings.Repeat("a", 32)
	existing := &peer{
		addr: key, tieKey: "z-connection", remoteIP: "8.8.8.8",
		outbound: true, connectedAt: time.Now(),
	}
	first := reserveOutbound(t, n, "seed.btc09.org:9009")
	if !n.registerPeer(first, key, "8.8.8.8:9009", existing) {
		t.Fatal("first peer was rejected")
	}

	attacker := &peer{
		addr: key, tieKey: "a-connection", remoteIP: "1.1.1.1",
		outbound: false, connectedAt: time.Now(),
	}
	second, ok := n.reserveInboundHandshake()
	if !ok {
		t.Fatal("could not reserve duplicate inbound handshake")
	}
	if n.registerPeer(second, key, "1.1.1.1:9009", attacker) {
		t.Fatal("different-source peer replaced an existing claimed node ID")
	}
	if got := n.peers[key]; got != existing {
		t.Fatalf("claimed node-ID owner = %p, want original %p", got, existing)
	}
}

func TestBroadcastClosesPeerAfterTerminalWriteFailure(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	conn := newFailingWriteConn()
	p := &peer{conn: conn, enc: bufio.NewWriter(conn), addr: "broken-peer"}
	n.peers[p.addr] = p

	n.broadcast(&Msg{Type: "ping"}, "")
	select {
	case <-conn.closed:
	case <-time.After(time.Second):
		t.Fatal("terminal broadcast write failure did not close peer connection")
	}
}

func TestBroadcastTxCountsOnlySuccessfulWrites(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	var written bytes.Buffer
	goodConn := &bufferConn{w: &written}
	badConn := newFailingWriteConn()
	n.peers["good"] = &peer{conn: goodConn, enc: bufio.NewWriter(goodConn), addr: "good"}
	n.peers["bad"] = &peer{conn: badConn, enc: bufio.NewWriter(badConn), addr: "bad"}
	tx := core.NewCoinbase(1, 1, [20]byte{1}, "count")
	if got := n.BroadcastTx(tx); got != 1 {
		t.Fatalf("BroadcastTx writes = %d, want 1", got)
	}
}

func TestWaitForPeersHonorsContext(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if n.WaitForPeers(ctx, 1) {
		t.Fatal("WaitForPeers reported a peer after context timeout")
	}
	n.peers["ready"] = &peer{addr: "ready"}
	if !n.WaitForPeers(context.Background(), 1) {
		t.Fatal("WaitForPeers did not observe existing peer")
	}
}

func TestTransactionReplayDoesNotBounceAroundCyclicPeers(t *testing.T) {
	chains := []*core.Chain{newBatchTestChain(t), newBatchTestChain(t), newBatchTestChain(t)}
	key, pkh := batchTestKey(t)
	for i := 0; i < 3; i++ {
		mineBatchTestBlock(t, chains[0], pkh)
		block := chains[0].BlockAt(int64(i + 1))
		for _, chain := range chains[1:] {
			if err := chain.AcceptBlock(block); err != nil {
				t.Fatal(err)
			}
		}
	}
	var outpoint core.OutPoint
	var entry core.UTXOEntry
	for outpoint, entry = range chains[0].UTXOsForPKH(pkh) {
		break
	}
	tx := &core.Tx{
		Version: 1, Ins: []core.TxIn{{Prev: outpoint}},
		Outs: []core.TxOut{{Value: entry.Value - 1, PubKeyHash: pkh}},
	}
	if err := tx.Sign([]ed25519.PrivateKey{key}); err != nil {
		t.Fatal(err)
	}
	nodes := []*Node{
		NewNode(chains[0], ":0", log.New(io.Discard, "", 0)),
		NewNode(chains[1], ":0", log.New(io.Discard, "", 0)),
		NewNode(chains[2], ":0", log.New(io.Discard, "", 0)),
	}
	buffers := []*bytes.Buffer{{}, {}, {}}
	for i, node := range nodes {
		conn := &bufferConn{w: buffers[i]}
		peerAddr := []string{"ab", "bc", "ca"}[i]
		node.peers[peerAddr] = &peer{conn: conn, enc: bufio.NewWriter(conn), addr: peerAddr}
	}
	raw := tx.Bytes()
	if err := nodes[0].handleMsg(&peer{addr: "ca"}, &Msg{Type: "tx", Raw: raw}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		message, err := readMsg(bufio.NewReader(buffers[i]))
		if err != nil {
			t.Fatalf("cycle hop %d: %v", i, err)
		}
		if err := nodes[i+1].handleMsg(&peer{addr: []string{"ab", "bc"}[i]}, message); err != nil {
			t.Fatal(err)
		}
	}
	message, err := readMsg(bufio.NewReader(buffers[2]))
	if err != nil {
		t.Fatal(err)
	}
	if err := nodes[0].handleMsg(&peer{addr: "ca"}, message); err != nil {
		t.Fatal(err)
	}
	if buffers[0].Len() != 0 {
		t.Fatalf("exact replay was regossiped around cycle (%d bytes)", buffers[0].Len())
	}
}

func TestSilentHandshakeTimesOutAndReleasesCapacity(t *testing.T) {
	n := NewNode(newBatchTestChain(t), ":0", log.New(io.Discard, "", 0))
	n.handshakeTimeout = 40 * time.Millisecond
	reservation, ok := n.reserveInboundHandshake()
	if !ok {
		t.Fatal("could not reserve inbound handshake")
	}
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		n.handleConn(context.Background(), server, reservation)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("silent handshake did not time out")
	}
	n.mu.RLock()
	pending := len(n.pending)
	n.mu.RUnlock()
	if pending != 0 {
		t.Fatalf("pending handshakes = %d after timeout", pending)
	}
}

func testListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

type deadlineObservingConn struct {
	net.Conn
	onClear func()
}

func (c *deadlineObservingConn) SetDeadline(deadline time.Time) error {
	if deadline.IsZero() && c.onClear != nil {
		c.onClear()
	}
	return c.Conn.SetDeadline(deadline)
}

type failingWriteConn struct {
	closed chan struct{}
	once   sync.Once
}

func newFailingWriteConn() *failingWriteConn {
	return &failingWriteConn{closed: make(chan struct{})}
}

func (c *failingWriteConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *failingWriteConn) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (c *failingWriteConn) LocalAddr() net.Addr              { return testAddr("local") }
func (c *failingWriteConn) RemoteAddr() net.Addr             { return testAddr("remote") }
func (c *failingWriteConn) SetDeadline(time.Time) error      { return nil }
func (c *failingWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (c *failingWriteConn) SetWriteDeadline(time.Time) error { return nil }
func (c *failingWriteConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func reserveOutbound(t *testing.T, n *Node, target string) handshakeReservation {
	t.Helper()
	n.mu.Lock()
	n.known[target] = true
	n.mu.Unlock()
	reservations := n.reserveDialCandidates()
	if len(reservations) != 1 || reservations[0].target != target {
		t.Fatalf("reservations = %#v, want target %q", reservations, target)
	}
	return reservations[0]
}

func getBlockBatch(t *testing.T, n *Node, from int64) []*Msg {
	t.Helper()
	var out bytes.Buffer
	conn := &bufferConn{w: &out}
	p := &peer{conn: conn, enc: bufio.NewWriter(conn), addr: "test-peer"}
	if err := n.handleMsg(p, &Msg{Type: "getblocks", From: from}); err != nil {
		t.Fatalf("handle getblocks: %v", err)
	}

	r := bufio.NewReader(&out)
	var msgs []*Msg
	for {
		msg, err := readMsg(r)
		if err != nil {
			break
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func newBatchTestChain(t *testing.T) *core.Chain {
	t.Helper()
	p := core.RegTest
	p.Name = "p2p-batch-test"
	p.ArgonMemKiB = 8
	p.MaxTargetBits = 0x2100ffff
	p.RetargetInterval = 1 << 30
	for {
		if core.GenesisBlock(&p).Header.CheckPow(&p) {
			break
		}
		p.GenesisNonce++
	}
	c, err := core.NewChain(&p)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	return c
}

func mineBatchTestBlock(t *testing.T, c *core.Chain, pkh [20]byte) {
	t.Helper()
	tmpl := core.BuildBlockTemplate(c, pkh, "p2p-test")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := core.Mine(ctx, c, tmpl, 1)
	if res.Block == nil {
		t.Fatal("mining test block timed out")
	}
	if err := c.AcceptBlock(res.Block); err != nil {
		t.Fatalf("AcceptBlock: %v", err)
	}
}

func batchTestKey(t *testing.T) (ed25519.PrivateKey, [20]byte) {
	t.Helper()
	k, err := core.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return k, core.PubKeyHash20(k.Public().(ed25519.PublicKey))
}

type bufferConn struct {
	w *bytes.Buffer
}

func (c *bufferConn) Read(_ []byte) (int, error)         { return 0, io.EOF }
func (c *bufferConn) Write(p []byte) (int, error)        { return c.w.Write(p) }
func (c *bufferConn) Close() error                       { return nil }
func (c *bufferConn) LocalAddr() net.Addr                { return testAddr("local") }
func (c *bufferConn) RemoteAddr() net.Addr               { return testAddr("remote") }
func (c *bufferConn) SetDeadline(_ time.Time) error      { return nil }
func (c *bufferConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *bufferConn) SetWriteDeadline(_ time.Time) error { return nil }

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }
