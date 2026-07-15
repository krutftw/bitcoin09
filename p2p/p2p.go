// Package p2p implements Bitcoin 09's peer-to-peer protocol: a small
// length-framed TCP protocol for exchanging blocks, transactions and peer
// addresses. Every message is length-prefixed JSON, deliberately simple
// and auditable; consensus security lives in core, not in transport.
package p2p

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

const (
	protocolVersion          = 1
	maxMsgBytes              = core.MaxBlockBytes + 4096
	maxPeers                 = 32
	reservedOutboundSlots    = 4
	maxPendingOutbound       = 4
	maxPendingInbound        = 8
	maxPendingInboundPerIP   = 2
	blockBatchSize           = 64
	blockBatchBytes          = 4 * 1024 * 1024
	defaultHandshakeTimeout  = 10 * time.Second
	duplicateCrossDialWindow = 15 * time.Second
)

type Msg struct {
	Type    string   `json:"type"`              // hello|inv|getblocks|block|tx|addr|ping|pong
	Version int      `json:"version,omitempty"` // hello
	Magic   uint32   `json:"magic,omitempty"`   // hello
	Height  int64    `json:"height,omitempty"`  // hello, inv, block
	Hashes  []string `json:"hashes,omitempty"`  // inv (block ids, hex)
	From    int64    `json:"from,omitempty"`    // getblocks: start height
	Raw     []byte   `json:"raw,omitempty"`     // block | tx wire bytes
	Addrs   []string `json:"addrs,omitempty"`   // addr gossip
	Listen  string   `json:"listen,omitempty"`  // hello: my listen addr
	NodeID  string   `json:"node_id,omitempty"` // hello: process-stable random identity
	Nonce   string   `json:"nonce,omitempty"`   // hello: per-connection tie breaker
	More    bool     `json:"more,omitempty"`    // block: sender has more blocks after this batch
}

// Node is a full node: chain + peers + gossip.
type Node struct {
	mu               sync.RWMutex
	chain            *core.Chain
	listen           string
	peers            map[string]*peer  // key: remote node ID or legacy identity
	known            map[string]bool   // addresses we can dial
	trustedSeeds     map[string]bool   // configured seeds, always dialed first
	seedTargets      []string          // configured seed order
	suppressed       map[string]bool   // targets proven to resolve back to this node
	dialing          map[string]uint64 // dial target -> owned handshake token
	pending          map[uint64]handshakeReservation
	targets          map[string]string // dial target -> connected peer key
	nextToken        uint64
	seedCursor       int
	gossipCursor     int
	nodeID           string
	handshakeTimeout time.Duration
	logger           *log.Logger
	shutdown         context.CancelFunc
}

type handshakeReservation struct {
	token    uint64
	target   string
	sourceIP string
}

type peer struct {
	conn             net.Conn
	enc              *bufio.Writer
	mu               sync.Mutex // serializes writes
	addr             string
	tieKey           string
	remoteIP         string
	outbound         bool
	connectedAt      time.Time
	advertisedHeight int64
}

func NewNode(chain *core.Chain, listen string, logger *log.Logger) *Node {
	n := &Node{
		chain:            chain,
		listen:           listen,
		peers:            make(map[string]*peer),
		known:            make(map[string]bool),
		trustedSeeds:     make(map[string]bool),
		suppressed:       make(map[string]bool),
		dialing:          make(map[string]uint64),
		pending:          make(map[uint64]handshakeReservation),
		targets:          make(map[string]string),
		nodeID:           randomHandshakeID(),
		handshakeTimeout: defaultHandshakeTimeout,
		logger:           logger,
	}
	chain.OnNewTip = func(b *core.Block, h int64) {
		go n.announceBlock(b, h)
	}
	return n
}

// Start listens and begins dialing seeds.
func (n *Node) Start(ctx context.Context, seeds []string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", n.listen)
	if err != nil {
		return err
	}
	return n.startWithListener(ctx, ln, seeds)
}

func (n *Node) startWithListener(parent context.Context, ln net.Listener, seeds []string) error {
	ctx, cancel := context.WithCancel(parent)
	n.shutdown = cancel
	n.logger.Printf("p2p listening on %s", ln.Addr())
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			reservation, ok := n.reserveInboundHandshakeFrom(c.RemoteAddr().String())
			if !ok {
				c.Close()
				continue
			}
			go n.handleConn(ctx, c, reservation)
		}
	}()
	n.mu.Lock()
	for _, s := range seeds {
		if target, ok := normalizeDialTarget(s); ok && !n.suppressed[target] {
			n.known[target] = true
			if !n.trustedSeeds[target] {
				n.trustedSeeds[target] = true
				n.seedTargets = append(n.seedTargets, target)
			}
		}
	}
	n.mu.Unlock()
	for _, reservation := range n.reserveDialCandidates() {
		go n.dial(ctx, reservation)
	}
	go n.reconnectLoop(ctx)
	go n.catchupLoop(ctx)
	return nil
}

func (n *Node) reconnectLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, reservation := range n.reserveDialCandidates() {
				go n.dial(ctx, reservation)
			}
		}
	}
}

func (n *Node) catchupLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, h := n.chain.Tip()
			n.broadcast(&Msg{Type: "getblocks", From: h + 1}, "")
		}
	}
}

func (n *Node) dial(ctx context.Context, reservation handshakeReservation) {
	defer n.releaseHandshake(reservation)
	d := net.Dialer{Timeout: 10 * time.Second}
	c, err := d.DialContext(ctx, "tcp", reservation.target)
	if err != nil {
		return
	}
	n.handleConn(ctx, c, reservation)
}

func (n *Node) reserveDialCandidates() []handshakeReservation {
	n.mu.Lock()
	defer n.mu.Unlock()

	pendingOutbound := 0
	for _, reservation := range n.pending {
		if reservation.target != "" {
			pendingOutbound++
		}
	}
	available := maxPeers - len(n.peers) - len(n.pending)
	if outboundAvailable := maxPendingOutbound - pendingOutbound; available > outboundAvailable {
		available = outboundAvailable
	}
	if available <= 0 {
		return nil
	}
	out := make([]handshakeReservation, 0, available)
	isAvailable := func(target string) bool {
		if !n.known[target] {
			return false
		}
		if n.suppressed[target] {
			delete(n.known, target)
			return false
		}
		if _, dialing := n.dialing[target]; dialing {
			return false
		}
		if key, ok := n.targets[target]; ok {
			if _, connected := n.peers[key]; connected {
				return false
			}
			delete(n.targets, target)
		}
		return true
	}
	reserve := func(target string) bool {
		if len(out) >= available || !isAvailable(target) {
			return false
		}
		reservation := n.reserveHandshakeLocked(target, "")
		out = append(out, reservation)
		return true
	}

	seedCandidates := make([]string, 0, len(n.seedTargets))
	for _, target := range n.seedTargets {
		if isAvailable(target) {
			seedCandidates = append(seedCandidates, target)
		}
	}

	gossipTargets := make([]string, 0, len(n.known))
	for target := range n.known {
		if !n.trustedSeeds[target] && isAvailable(target) {
			gossipTargets = append(gossipTargets, target)
		}
	}
	sort.Strings(gossipTargets)
	reserveRotating := func(candidates []string, cursor *int, limit int) {
		if len(candidates) == 0 || limit <= 0 {
			return
		}
		start := *cursor % len(candidates)
		added := 0
		for offset := 0; offset < len(candidates) && added < limit && len(out) < available; offset++ {
			index := (start + offset) % len(candidates)
			if reserve(candidates[index]) {
				added++
				*cursor = (index + 1) % len(candidates)
			}
		}
	}

	if len(seedCandidates) > 0 && len(gossipTargets) > 0 && available > 1 {
		reserveRotating(seedCandidates, &n.seedCursor, available-1)
		reserveRotating(gossipTargets, &n.gossipCursor, 1)
	} else if len(seedCandidates) > 0 {
		reserveRotating(seedCandidates, &n.seedCursor, available)
	} else {
		reserveRotating(gossipTargets, &n.gossipCursor, available)
	}
	if len(out) < available {
		reserveRotating(seedCandidates, &n.seedCursor, available-len(out))
	}
	if len(out) < available {
		reserveRotating(gossipTargets, &n.gossipCursor, available-len(out))
	}
	return out
}

func (n *Node) reserveInboundHandshake() (handshakeReservation, bool) {
	return n.reserveInboundHandshakeFrom("")
}

func (n *Node) reserveInboundHandshakeFrom(remoteAddr string) (handshakeReservation, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	sourceIP := remoteIPFromAddr(remoteAddr)
	inboundPeers := 0
	pendingInbound := 0
	pendingFromSource := 0
	for _, p := range n.peers {
		if !p.outbound {
			inboundPeers++
		}
	}
	for _, reservation := range n.pending {
		if reservation.target != "" {
			continue
		}
		pendingInbound++
		if sourceIP != "" && reservation.sourceIP == sourceIP {
			pendingFromSource++
		}
	}
	if len(n.peers)+len(n.pending) >= maxPeers ||
		inboundPeers+pendingInbound >= maxPeers-reservedOutboundSlots ||
		pendingInbound >= maxPendingInbound ||
		(sourceIP != "" && pendingFromSource >= maxPendingInboundPerIP) {
		return handshakeReservation{}, false
	}
	return n.reserveHandshakeLocked("", sourceIP), true
}

func (n *Node) reserveHandshakeLocked(target, sourceIP string) handshakeReservation {
	n.nextToken++
	reservation := handshakeReservation{token: n.nextToken, target: target, sourceIP: sourceIP}
	n.pending[reservation.token] = reservation
	if target != "" {
		n.dialing[target] = reservation.token
	}
	return reservation
}

func (n *Node) releaseHandshake(reservation handshakeReservation) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.releaseHandshakeLocked(reservation)
}

func (n *Node) releaseHandshakeLocked(reservation handshakeReservation) bool {
	current, ok := n.pending[reservation.token]
	if !ok || current != reservation {
		return false
	}
	delete(n.pending, reservation.token)
	if reservation.target != "" && n.dialing[reservation.target] == reservation.token {
		delete(n.dialing, reservation.target)
	}
	return true
}

func (n *Node) registerPeer(reservation handshakeReservation, key, dialable string, p *peer) bool {
	n.mu.Lock()
	if !n.releaseHandshakeLocked(reservation) {
		n.mu.Unlock()
		return false
	}
	p.outbound = reservation.target != ""
	if p.connectedAt.IsZero() {
		p.connectedAt = time.Now()
	}
	if existing, exists := n.peers[key]; exists {
		sameSource := p.remoteIP != "" && p.remoteIP == existing.remoteIP
		if sameSource {
			n.rememberPeerTargetsLocked(reservation.target, dialable, key)
		}
		if !canReplaceConcurrentDuplicate(existing, p) {
			n.mu.Unlock()
			return false
		}
		n.peers[key] = p
		n.mu.Unlock()
		if existing.conn != nil {
			existing.conn.Close()
		}
		return true
	}
	if len(n.peers) >= maxPeers {
		n.mu.Unlock()
		return false
	}
	n.peers[key] = p
	n.rememberPeerTargetsLocked(reservation.target, dialable, key)
	n.mu.Unlock()
	return true
}

func (n *Node) rememberPeerTargetsLocked(target, dialable, key string) {
	if target != "" && !n.suppressed[target] {
		n.targets[target] = key
	}
	if dialable != "" && !n.suppressed[dialable] {
		n.targets[dialable] = key
		if len(n.known) < 1000 {
			n.known[dialable] = true
		}
	}
}

func canReplaceConcurrentDuplicate(existing, candidate *peer) bool {
	if existing.remoteIP == "" || candidate.remoteIP == "" || existing.remoteIP != candidate.remoteIP {
		return false
	}
	if existing.tieKey == "" || candidate.tieKey == "" || candidate.tieKey >= existing.tieKey {
		return false
	}
	delta := candidate.connectedAt.Sub(existing.connectedAt)
	if delta < 0 {
		delta = -delta
	}
	return delta <= duplicateCrossDialWindow
}

func (n *Node) unregisterPeer(key string, owner *peer) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.peers[key] != owner {
		return false
	}
	delete(n.peers, key)
	for target, peerKey := range n.targets {
		if peerKey == key {
			delete(n.targets, target)
		}
	}
	return true
}

func (n *Node) handleConn(ctx context.Context, conn net.Conn, reservation handshakeReservation) {
	defer n.releaseHandshake(reservation)
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopClose()

	localNonce := randomHandshakeID()
	p := &peer{
		conn: conn, enc: bufio.NewWriter(conn), addr: conn.RemoteAddr().String(),
		remoteIP: remoteIPFromAddr(conn.RemoteAddr().String()), connectedAt: time.Now(),
	}
	if err := conn.SetDeadline(time.Now().Add(n.handshakeTimeout)); err != nil {
		return
	}

	_, tipH := n.chain.Tip()
	if err := p.sendWithTimeout(&Msg{
		Type: "hello", Version: protocolVersion, Magic: n.chain.Params().NetMagic,
		Height: tipH, Listen: n.listen, NodeID: n.nodeID, Nonce: localNonce,
	}, n.handshakeTimeout); err != nil {
		return
	}
	r := bufio.NewReader(conn)
	hello, err := readMsg(r)
	if err != nil || hello.Type != "hello" || hello.Magic != n.chain.Params().NetMagic {
		return // wrong network or protocol
	}
	if hello.Height > 0 {
		p.advertisedHeight = hello.Height
	}
	dialable, _ := dialablePeerEndpoint(p.addr, hello.Listen)
	if remoteID, ok := normalizeHandshakeID(hello.NodeID); ok && remoteID == n.nodeID {
		n.suppressSelfTargets(reservation.target, dialable)
		return // self-connection through an advertised alias
	}

	key := peerIdentity(hello.NodeID, p.addr, dialable)
	p.addr = key
	p.tieKey = connectionTieKey(n.nodeID, localNonce, hello.NodeID, hello.Nonce)
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return
	}
	if !n.registerPeer(reservation, key, dialable, p) {
		return
	}
	n.logger.Printf("peer connected: %s (height %d)", key, hello.Height)
	defer func() {
		if n.unregisterPeer(key, p) {
			n.logger.Printf("peer gone: %s", key)
		}
	}()

	// if they're ahead, ask for blocks
	if _, h := n.chain.Tip(); hello.Height > h {
		if err := p.send(&Msg{Type: "getblocks", From: h + 1}); err != nil {
			return
		}
	}
	// share known addresses
	n.mu.RLock()
	var addrs []string
	for a := range n.known {
		if normalized, ok := normalizeGossipAddress(a); ok {
			addrs = append(addrs, normalized)
		}
		if len(addrs) >= 16 {
			break
		}
	}
	n.mu.RUnlock()
	if err := p.send(&Msg{Type: "addr", Addrs: addrs}); err != nil {
		return
	}

	// keepalive so idle connections outlive the read deadline
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()
	go func() {
		t := time.NewTicker(90 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-t.C:
				if p.send(&Msg{Type: "ping"}) != nil {
					p.conn.Close()
					return
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		m, err := readMsg(r)
		if err != nil {
			return
		}
		if err := n.handleMsg(p, m); err != nil {
			n.logger.Printf("peer %s: %v", key, err)
			return
		}
	}
}

func (n *Node) handleMsg(p *peer, m *Msg) error {
	switch m.Type {
	case "ping":
		return p.send(&Msg{Type: "pong"})
	case "pong":
		return nil
	case "addr":
		n.mu.Lock()
		for _, a := range m.Addrs {
			if normalized, ok := normalizeGossipAddress(a); ok && len(n.known) < 1000 && normalized != n.listen && !n.suppressed[normalized] {
				n.known[normalized] = true
			}
		}
		n.mu.Unlock()
		return nil
	case "getblocks":
		// Send a bounded batch. This avoids the old unbounded stream while
		// still catching up much faster than one block per round trip.
		type batchBlock struct {
			height int64
			raw    []byte
		}
		var batch []batchBlock
		totalBytes := 0
		for h := m.From; len(batch) < blockBatchSize; h++ {
			blk := n.chain.BlockAt(h)
			if blk == nil {
				break
			}
			raw := blk.Bytes()
			if len(batch) > 0 && totalBytes+len(raw) > blockBatchBytes {
				break
			}
			batch = append(batch, batchBlock{height: h, raw: raw})
			totalBytes += len(raw)
		}
		for i, blk := range batch {
			last := i == len(batch)-1
			more := last && n.chain.BlockAt(blk.height+1) != nil
			if err := p.send(&Msg{Type: "block", Raw: blk.raw, Height: blk.height, More: more}); err != nil {
				return err
			}
		}
		return nil
	case "block":
		n.notePeerHeight(p, m.Height)
		blk, err := core.DecodeBlock(m.Raw)
		if err != nil {
			return fmt.Errorf("bad block: %w", err)
		}
		err = n.chain.AcceptBlock(blk)
		switch {
		case err == nil:
			if m.More {
				return p.send(&Msg{Type: "getblocks", From: m.Height + 1})
			}
			return nil
		case err.Error() == "orphan: unknown parent":
			// The peer may be on a heavier fork that split before our tip.
			// Walk back to genesis and rebuild the side branch instead of
			// asking for tip+1 forever.
			return p.send(&Msg{Type: "getblocks", From: 1})
		default:
			// Invalid blocks mean the peer is on another fork or broken.
			return fmt.Errorf("invalid block: %w", err)
		}
	case "tx":
		tx, err := core.DecodeTx(m.Raw)
		if err != nil {
			return fmt.Errorf("bad tx: %w", err)
		}
		if result, err := n.chain.AcceptTxWithResult(tx); err == nil && result == core.TxAcceptanceAdded {
			n.broadcast(&Msg{Type: "tx", Raw: m.Raw}, p.addr)
		}
		return nil
	case "inv":
		n.notePeerHeight(p, m.Height)
		// simple protocol: inv of a new tip triggers getblocks if ahead
		if _, h := n.chain.Tip(); m.Height > h {
			return p.send(&Msg{Type: "getblocks", From: h + 1})
		}
		return nil
	}
	return nil
}

func normalizeDialTarget(addr string) (string, bool) {
	addr = strings.TrimSpace(addr)
	if addr == "" || len(addr) > 320 {
		return "", false
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return "", false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", false
	}
	if ip := net.ParseIP(host); ip != nil {
		return net.JoinHostPort(ip.String(), strconv.Itoa(port)), true
	}
	if len(host) > 253 || strings.ContainsAny(host, " /\\\t\r\n") {
		return "", false
	}
	return net.JoinHostPort(strings.ToLower(strings.TrimSuffix(host, ".")), strconv.Itoa(port)), true
}

func normalizeGossipAddress(addr string) (string, bool) {
	normalized, ok := normalizeDialTarget(addr)
	if !ok {
		return "", false
	}
	host, port, _ := net.SplitHostPort(normalized)
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", false
	}
	if ip.Zone() != "" {
		return "", false
	}
	ip = ip.Unmap()
	if !isPublicGossipIP(ip) {
		return "", false
	}
	return net.JoinHostPort(ip.String(), port), true
}

var ipv6GlobalUnicastPrefix = netip.MustParsePrefix("2000::/3")

var blockedGossipPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3ffe::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

func isPublicGossipIP(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip.Is6() && !ipv6GlobalUnicastPrefix.Contains(ip) {
		return false
	}
	for _, prefix := range blockedGossipPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func dialablePeerEndpoint(remoteAddr, advertisedListen string) (string, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil || host == "" {
		return "", false
	}
	_, portText, err := net.SplitHostPort(advertisedListen)
	if err != nil {
		return "", false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", false
	}
	return normalizeGossipAddress(net.JoinHostPort(host, strconv.Itoa(port)))
}

func remoteIPFromAddr(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return ""
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	return ip.Unmap().String()
}

func (n *Node) suppressSelfTargets(targets ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, target := range targets {
		if target == "" {
			continue
		}
		n.suppressed[target] = true
		delete(n.known, target)
		delete(n.targets, target)
	}
}

func peerIdentity(remoteNodeID, remoteAddr, dialable string) string {
	if nodeID, ok := normalizeHandshakeID(remoteNodeID); ok {
		if sourceIP := remoteIPFromAddr(remoteAddr); sourceIP != "" {
			return "node:" + nodeID + "@" + sourceIP
		}
		return "node:" + nodeID
	}
	if dialable != "" {
		return "legacy:" + dialable
	}
	return "legacy-conn:" + remoteAddr
}

func connectionTieKey(localNodeID, localNonce, remoteNodeID, remoteNonce string) string {
	localID, localIDOK := normalizeHandshakeID(localNodeID)
	localNonce, localNonceOK := normalizeHandshakeID(localNonce)
	remoteID, remoteIDOK := normalizeHandshakeID(remoteNodeID)
	remoteNonce, remoteNonceOK := normalizeHandshakeID(remoteNonce)
	if !localIDOK || !localNonceOK || !remoteIDOK || !remoteNonceOK {
		return ""
	}
	local := localID + ":" + localNonce
	remote := remoteID + ":" + remoteNonce
	if local > remote {
		local, remote = remote, local
	}
	return local + "|" + remote
}

func normalizeHandshakeID(value string) (string, bool) {
	if len(value) != 32 {
		return "", false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 16 {
		return "", false
	}
	return strings.ToLower(value), true
}

func randomHandshakeID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic(fmt.Sprintf("p2p: random identity: %v", err))
	}
	return hex.EncodeToString(raw)
}

// announceBlock gossips a new tip to all peers.
func (n *Node) announceBlock(b *core.Block, height int64) {
	n.broadcast(&Msg{Type: "block", Raw: b.Bytes(), Height: height}, "")
}

// BroadcastTx gossips a locally-submitted transaction and returns the number
// of peers that accepted the complete framed write.
func (n *Node) BroadcastTx(tx *core.Tx) int {
	if tx == nil {
		return 0
	}
	return n.broadcast(&Msg{Type: "tx", Raw: tx.Bytes()}, "")
}

func (n *Node) broadcast(m *Msg, exceptAddr string) int {
	n.mu.RLock()
	ps := make([]*peer, 0, len(n.peers))
	for _, p := range n.peers {
		if p.addr != exceptAddr {
			ps = append(ps, p)
		}
	}
	n.mu.RUnlock()
	writes := 0
	for _, p := range ps {
		if err := p.send(m); err != nil {
			if p.conn != nil {
				p.conn.Close()
			}
			continue
		}
		writes++
	}
	return writes
}

// PeerCount returns the number of connected peers.
func (n *Node) PeerCount() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.peers)
}

func (n *Node) notePeerHeight(p *peer, height int64) {
	if p == nil || height < 0 {
		return
	}
	n.mu.Lock()
	if height > p.advertisedHeight {
		p.advertisedHeight = height
	}
	n.mu.Unlock()
}

// HighestAdvertisedHeight returns the largest untrusted tip height reported by
// a currently connected peer. It is health telemetry, not consensus input.
func (n *Node) HighestAdvertisedHeight() int64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	var highest int64
	for _, peer := range n.peers {
		if peer.advertisedHeight > highest {
			highest = peer.advertisedHeight
		}
	}
	return highest
}

// WaitForPeers waits until at least minimum peers are connected or ctx is
// cancelled. Polling is deliberately bounded and does not retain peer locks.
func (n *Node) WaitForPeers(ctx context.Context, minimum int) bool {
	if minimum <= 0 {
		return true
	}
	if ctx == nil {
		return false
	}
	if n.PeerCount() >= minimum {
		return true
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if n.PeerCount() >= minimum {
				return true
			}
		}
	}
}

// ---- framing: 4-byte big-endian length + JSON ----

func (p *peer) send(m *Msg) error {
	return p.sendWithTimeout(m, 2*time.Minute)
}

func (p *peer) sendWithTimeout(m *Msg, timeout time.Duration) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > maxMsgBytes {
		return errors.New("message too large")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var lenb [4]byte
	binary.BigEndian.PutUint32(lenb[:], uint32(len(b)))
	if err := p.conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if _, err := p.enc.Write(lenb[:]); err != nil {
		return err
	}
	if _, err := p.enc.Write(b); err != nil {
		return err
	}
	return p.enc.Flush()
}

func readMsg(r *bufio.Reader) (*Msg, error) {
	var lenb [4]byte
	if _, err := io.ReadFull(r, lenb[:]); err != nil {
		return nil, err
	}
	ln := binary.BigEndian.Uint32(lenb[:])
	if ln > maxMsgBytes {
		return nil, errors.New("oversize message")
	}
	buf := make([]byte, ln)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	m := &Msg{}
	if err := json.Unmarshal(buf, m); err != nil {
		return nil, err
	}
	return m, nil
}
