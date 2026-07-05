package core

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Header is the 88-byte mined portion of a block.
type Header struct {
	Version    uint32
	PrevBlock  Hash32
	MerkleRoot Hash32
	Time       int64
	Bits       uint32
	Nonce      uint64
}

// Bytes returns the canonical 88-byte header encoding (the PoW input).
func (h *Header) Bytes() []byte {
	buf := make([]byte, 0, 88)
	var u32 [4]byte
	var u64 [8]byte
	binary.LittleEndian.PutUint32(u32[:], h.Version)
	buf = append(buf, u32[:]...)
	buf = append(buf, h.PrevBlock[:]...)
	buf = append(buf, h.MerkleRoot[:]...)
	binary.LittleEndian.PutUint64(u64[:], uint64(h.Time))
	buf = append(buf, u64[:]...)
	binary.LittleEndian.PutUint32(u32[:], h.Bits)
	buf = append(buf, u32[:]...)
	binary.LittleEndian.PutUint64(u64[:], h.Nonce)
	buf = append(buf, u64[:]...)
	return buf
}

// ID is the block identifier: cheap SHA256d of the header.
// (PoW validity uses PowHash separately; ids stay cheap to compute.)
func (h *Header) ID() Hash32 { return SHA256d(h.Bytes()) }

// Block is a header plus its transactions (first must be the coinbase).
type Block struct {
	Header Header
	Txs    []*Tx
}

// MerkleRoot computes the Bitcoin-style merkle root of txids
// (last node duplicated on odd levels).
func MerkleRoot(txs []*Tx) Hash32 {
	if len(txs) == 0 {
		return Hash32{}
	}
	layer := make([]Hash32, len(txs))
	for i, t := range txs {
		layer[i] = t.ID()
	}
	for len(layer) > 1 {
		if len(layer)%2 == 1 {
			layer = append(layer, layer[len(layer)-1])
		}
		next := make([]Hash32, 0, len(layer)/2)
		for i := 0; i < len(layer); i += 2 {
			cat := append(append([]byte{}, layer[i][:]...), layer[i+1][:]...)
			next = append(next, SHA256d(cat))
		}
		layer = next
	}
	return layer[0]
}

// Bytes encodes the full block.
func (b *Block) Bytes() []byte {
	buf := new(bytes.Buffer)
	buf.Write(b.Header.Bytes())
	buf.Write(putUvarint(nil, uint64(len(b.Txs))))
	for _, t := range b.Txs {
		writeBytes(buf, t.Bytes())
	}
	return buf.Bytes()
}

// DecodeBlock parses a block from bytes.
func DecodeBlock(raw []byte) (*Block, error) {
	if len(raw) < 88 {
		return nil, errors.New("short block")
	}
	if len(raw) > MaxBlockBytes+16 {
		return nil, errors.New("oversize block")
	}
	h := Header{}
	h.Version = binary.LittleEndian.Uint32(raw[0:4])
	copy(h.PrevBlock[:], raw[4:36])
	copy(h.MerkleRoot[:], raw[36:68])
	h.Time = int64(binary.LittleEndian.Uint64(raw[68:76]))
	h.Bits = binary.LittleEndian.Uint32(raw[76:80])
	h.Nonce = binary.LittleEndian.Uint64(raw[80:88])
	r := bytes.NewReader(raw[88:])
	n, err := binary.ReadUvarint(r)
	if err != nil || n == 0 || n > 100_000 {
		return nil, fmt.Errorf("bad tx count: %w", err)
	}
	blk := &Block{Header: h}
	for i := uint64(0); i < n; i++ {
		txb, err := readBytes(r, MaxBlockBytes)
		if err != nil {
			return nil, err
		}
		tx, err := DecodeTx(txb)
		if err != nil {
			return nil, err
		}
		blk.Txs = append(blk.Txs, tx)
	}
	if r.Len() != 0 {
		return nil, errors.New("trailing bytes in block")
	}
	return blk, nil
}

// CheckPow verifies the header's Argon2id hash meets its claimed target.
func (h *Header) CheckPow(p *Params) bool {
	target := CompactToTarget(h.Bits)
	if target.Sign() <= 0 || target.Cmp(p.MaxTarget()) > 0 {
		return false
	}
	return HashToBig(PowHash(h.Bytes(), p)).Cmp(target) <= 0
}

// CheckTimestamp enforces the future-drift rule.
func (h *Header) CheckTimestamp(p *Params, now time.Time) error {
	if h.Time > now.Unix()+p.FutureDrift {
		return errors.New("block timestamp too far in the future")
	}
	return nil
}
