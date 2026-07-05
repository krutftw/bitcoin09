package core

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
)

// OutPoint references a specific output of a prior transaction.
type OutPoint struct {
	TxID Hash32
	Idx  uint32
}

// TxIn spends an outpoint. Sig covers the transaction's signing digest and
// PubKey must hash to the PubKeyHash of the referenced output.
type TxIn struct {
	Prev   OutPoint
	PubKey []byte // 32-byte Ed25519 public key
	Sig    []byte // 64-byte Ed25519 signature
}

// TxOut locks `Value` units to a public key hash.
type TxOut struct {
	Value      int64
	PubKeyHash [20]byte
}

// Tx is a transaction. A coinbase has exactly one input with a zero Prev
// and height+arbitrary bytes in PubKey (no signature required).
type Tx struct {
	Version uint32
	Ins     []TxIn
	Outs    []TxOut
	LockTag []byte // coinbase-only extra data (headline, miner tag)
}

// IsCoinbase reports whether tx is a coinbase transaction.
func (t *Tx) IsCoinbase() bool {
	return len(t.Ins) == 1 && t.Ins[0].Prev.TxID == (Hash32{}) && t.Ins[0].Prev.Idx == 0xffffffff
}

func writeBytes(buf *bytes.Buffer, b []byte) {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], uint64(len(b)))
	buf.Write(tmp[:n])
	buf.Write(b)
}

func readBytes(r *bytes.Reader, maxLen uint64) ([]byte, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	if n > maxLen {
		return nil, errors.New("field too long")
	}
	b := make([]byte, n)
	if _, err := r.Read(b); err != nil && n > 0 {
		return nil, err
	}
	return b, nil
}

// serialize writes the tx; if forSigning, all Sig fields are omitted so the
// digest commits to everything except the signatures themselves.
func (t *Tx) serialize(forSigning bool) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, t.Version)
	buf.Write(putUvarint(nil, uint64(len(t.Ins))))
	for _, in := range t.Ins {
		buf.Write(in.Prev.TxID[:])
		binary.Write(buf, binary.LittleEndian, in.Prev.Idx)
		writeBytes(buf, in.PubKey)
		if forSigning {
			writeBytes(buf, nil)
		} else {
			writeBytes(buf, in.Sig)
		}
	}
	buf.Write(putUvarint(nil, uint64(len(t.Outs))))
	for _, out := range t.Outs {
		binary.Write(buf, binary.LittleEndian, uint64(out.Value))
		buf.Write(out.PubKeyHash[:])
	}
	writeBytes(buf, t.LockTag)
	return buf.Bytes()
}

// Bytes returns the full wire encoding.
func (t *Tx) Bytes() []byte { return t.serialize(false) }

// ID returns the txid: SHA256d over the full encoding.
func (t *Tx) ID() Hash32 { return SHA256d(t.Bytes()) }

// SigDigest returns the digest each input signs.
func (t *Tx) SigDigest() Hash32 { return SHA256d(t.serialize(true)) }

// Sign signs every input with the corresponding private key.
func (t *Tx) Sign(keys []ed25519.PrivateKey) error {
	if len(keys) != len(t.Ins) {
		return errors.New("key count mismatch")
	}
	digest := t.SigDigest()
	for i := range t.Ins {
		pub := keys[i].Public().(ed25519.PublicKey)
		t.Ins[i].PubKey = append([]byte(nil), pub...)
	}
	// PubKeys are part of the digest; recompute after setting them.
	digest = t.SigDigest()
	for i := range t.Ins {
		t.Ins[i].Sig = ed25519.Sign(keys[i], digest[:])
	}
	return nil
}

// DecodeTx parses a transaction from bytes.
func DecodeTx(b []byte) (*Tx, error) {
	r := bytes.NewReader(b)
	t := &Tx{}
	if err := binary.Read(r, binary.LittleEndian, &t.Version); err != nil {
		return nil, err
	}
	nIn, err := binary.ReadUvarint(r)
	if err != nil || nIn > 10_000 {
		return nil, fmt.Errorf("bad input count: %w", err)
	}
	for i := uint64(0); i < nIn; i++ {
		var in TxIn
		if _, err := r.Read(in.Prev.TxID[:]); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.LittleEndian, &in.Prev.Idx); err != nil {
			return nil, err
		}
		if in.PubKey, err = readBytes(r, 128); err != nil {
			return nil, err
		}
		if in.Sig, err = readBytes(r, 128); err != nil {
			return nil, err
		}
		t.Ins = append(t.Ins, in)
	}
	nOut, err := binary.ReadUvarint(r)
	if err != nil || nOut > 10_000 {
		return nil, fmt.Errorf("bad output count: %w", err)
	}
	for i := uint64(0); i < nOut; i++ {
		var out TxOut
		var v uint64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		out.Value = int64(v)
		if _, err := r.Read(out.PubKeyHash[:]); err != nil {
			return nil, err
		}
		t.Outs = append(t.Outs, out)
	}
	if t.LockTag, err = readBytes(r, 256); err != nil {
		return nil, err
	}
	if r.Len() != 0 {
		return nil, errors.New("trailing bytes in tx")
	}
	return t, nil
}

// NewCoinbase builds the miner-reward transaction for a block.
func NewCoinbase(height int64, value int64, to [20]byte, tag string) *Tx {
	heightBytes := putUvarint(nil, uint64(height))
	return &Tx{
		Version: 1,
		Ins: []TxIn{{
			Prev:   OutPoint{TxID: Hash32{}, Idx: 0xffffffff},
			PubKey: heightBytes, // commits to height => unique txid per block
		}},
		Outs:    []TxOut{{Value: value, PubKeyHash: to}},
		LockTag: []byte(tag),
	}
}
