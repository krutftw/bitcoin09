// Package p2p implements Bitcoin 09's peer-to-peer protocol: a small
// length-framed TCP protocol for exchanging blocks, transactions and peer
// addresses. Every message is length-prefixed JSON, deliberately simple
// and auditable; consensus security lives in core, not in transport.
package p2p

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

const (
	protocolVersion = 1
	maxMsgBytes     = core.MaxBlockBytes + 4096
	maxPeers        = 32
	blockBatchSize  = 1
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
	More    bool     `json:"more,omitempty"`    // block: sender has more blocks after this batch
}

// Node is a full node: chain + peers + gossip.
type Node struct {
	mu       sync.RWMutex
	chain    *core.Chain
	listen   string
	peers    map[string]*peer // key: remote addr string
	known    map[string]bool  // addresses we can dial
	logger   *log.Logger
	shutdown context.CancelFunc
}

type peer struct {
	conn net.Conn
	enc  *bufio.Writer
	mu   sync.Mutex // serializes writes
	addr string
}

func NewNode(chain *core.Chain, listen string, logger *log.Logger) *Node {
	n := &Node{
		chain:  chain,
		listen: listen,
		peers:  make(map[string]*peer),
		known:  make(map[string]bool),
		logger: logger,
	}
	chain.OnNewTip = func(b *core.Block, h int64) {
		go n.announceBlock(b, h)
	}
	return n
}

// Start listens and begins dialing seeds.
func (n *Node) Start(ctx context.Context, seeds []string) error {
	ctx, cancel := context.WithCancel(ctx)
	n.shutdown = cancel
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", n.listen)
	if err != nil {
		return err
	}
	n.logger.Printf("p2p listening on %s", n.listen)
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
			go n.handleConn(ctx, c, false)
		}
	}()
	for _, s := range seeds {
		if s != "" {
			go n.dial(ctx, s)
		}
	}
	go n.reconnectLoop(ctx)
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
			n.mu.RLock()
			var dialable []string
			if len(n.peers) < maxPeers {
				for a := range n.known {
					if _, connected := n.peers[a]; !connected {
						dialable = append(dialable, a)
					}
				}
			}
			n.mu.RUnlock()
			for _, a := range dialable {
				go n.dial(ctx, a)
			}
		}
	}
}

func (n *Node) dial(ctx context.Context, addr string) {
	n.mu.Lock()
	if _, ok := n.peers[addr]; ok || len(n.peers) >= maxPeers {
		n.mu.Unlock()
		return
	}
	n.known[addr] = true
	n.mu.Unlock()
	d := net.Dialer{Timeout: 10 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return
	}
	n.handleConn(ctx, c, true)
}

func (n *Node) handleConn(ctx context.Context, conn net.Conn, outbound bool) {
	p := &peer{conn: conn, enc: bufio.NewWriter(conn), addr: conn.RemoteAddr().String()}
	defer conn.Close()

	_, tipH := n.chain.Tip()
	if err := p.send(&Msg{Type: "hello", Version: protocolVersion, Magic: n.chain.Params().NetMagic, Height: tipH, Listen: n.listen}); err != nil {
		return
	}
	r := bufio.NewReader(conn)
	hello, err := readMsg(r)
	if err != nil || hello.Type != "hello" || hello.Magic != n.chain.Params().NetMagic {
		return // wrong network or protocol
	}

	key := p.addr
	n.mu.Lock()
	if len(n.peers) >= maxPeers {
		n.mu.Unlock()
		return
	}
	n.peers[key] = p
	// remember the peer's dialable listen address if it shared one
	if hello.Listen != "" {
		if host, _, _ := net.SplitHostPort(p.addr); host != "" {
			if _, port, err := net.SplitHostPort(hello.Listen); err == nil {
				n.known[net.JoinHostPort(host, port)] = true
			}
		}
	}
	n.mu.Unlock()
	n.logger.Printf("peer connected: %s (height %d)", key, hello.Height)
	defer func() {
		n.mu.Lock()
		delete(n.peers, key)
		n.mu.Unlock()
		n.logger.Printf("peer gone: %s", key)
	}()

	// if they're ahead, ask for blocks
	if _, h := n.chain.Tip(); hello.Height > h {
		p.send(&Msg{Type: "getblocks", From: h + 1})
	}
	// share known addresses
	n.mu.RLock()
	var addrs []string
	for a := range n.known {
		addrs = append(addrs, a)
		if len(addrs) >= 16 {
			break
		}
	}
	n.mu.RUnlock()
	p.send(&Msg{Type: "addr", Addrs: addrs})

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
			if len(n.known) < 1000 && a != n.listen {
				n.known[a] = true
			}
		}
		n.mu.Unlock()
		return nil
	case "getblocks":
		// Send a bounded batch. Early versions streamed the whole missing
		// chain at once, which backed up TCP send queues on slow peers.
		sent := 0
		for h := m.From; sent < blockBatchSize; h++ {
			blk := n.chain.BlockAt(h)
			if blk == nil {
				break
			}
			next := n.chain.BlockAt(h + 1)
			if err := p.send(&Msg{Type: "block", Raw: blk.Bytes(), Height: h, More: sent == blockBatchSize-1 && next != nil}); err != nil {
				return err
			}
			sent++
		}
		return nil
	case "block":
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
		if err := n.chain.AcceptTx(tx); err == nil {
			n.broadcast(&Msg{Type: "tx", Raw: m.Raw}, p.addr)
		}
		return nil
	case "inv":
		// simple protocol: inv of a new tip triggers getblocks if ahead
		if _, h := n.chain.Tip(); m.Height > h {
			return p.send(&Msg{Type: "getblocks", From: h + 1})
		}
		return nil
	}
	return nil
}

// announceBlock gossips a new tip to all peers.
func (n *Node) announceBlock(b *core.Block, height int64) {
	n.broadcast(&Msg{Type: "block", Raw: b.Bytes(), Height: height}, "")
}

// BroadcastTx gossips a locally-submitted transaction.
func (n *Node) BroadcastTx(tx *core.Tx) {
	n.broadcast(&Msg{Type: "tx", Raw: tx.Bytes()}, "")
}

func (n *Node) broadcast(m *Msg, exceptAddr string) {
	n.mu.RLock()
	ps := make([]*peer, 0, len(n.peers))
	for _, p := range n.peers {
		if p.addr != exceptAddr {
			ps = append(ps, p)
		}
	}
	n.mu.RUnlock()
	for _, p := range ps {
		p.send(m)
	}
}

// PeerCount returns the number of connected peers.
func (n *Node) PeerCount() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.peers)
}

// ---- framing: 4-byte big-endian length + JSON ----

func (p *peer) send(m *Msg) error {
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
	p.conn.SetWriteDeadline(time.Now().Add(2 * time.Minute))
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
