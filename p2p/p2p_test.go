package p2p

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"io"
	"log"
	"net"
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
