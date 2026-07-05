// Package wallet implements a minimal key store and coin selection for
// Bitcoin 09. Keys are Ed25519 seeds stored 0600 in the data directory.
package wallet

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/krutftw/bitcoin09/core"
)

type keyFile struct {
	Keys []string `json:"keys"` // hex ed25519 seeds
}

// Wallet holds keys and derives addresses.
type Wallet struct {
	path string
	keys []ed25519.PrivateKey
}

// Load opens or creates a wallet file.
func Load(path string) (*Wallet, error) {
	w := &Wallet{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := w.NewAddress(); err != nil {
			return nil, err
		}
		return w, nil
	}
	if err != nil {
		return nil, err
	}
	var kf keyFile
	if err := json.Unmarshal(b, &kf); err != nil {
		return nil, err
	}
	for _, h := range kf.Keys {
		seed, err := hex.DecodeString(h)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("corrupt key in wallet")
		}
		w.keys = append(w.keys, ed25519.NewKeyFromSeed(seed))
	}
	if len(w.keys) == 0 {
		if _, err := w.NewAddress(); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func (w *Wallet) save() error {
	kf := keyFile{}
	for _, k := range w.keys {
		kf.Keys = append(kf.Keys, hex.EncodeToString(k.Seed()))
	}
	b, _ := json.MarshalIndent(kf, "", "  ")
	if err := os.MkdirAll(filepath.Dir(w.path), 0700); err != nil {
		return err
	}
	return os.WriteFile(w.path, b, 0600)
}

// NewAddress creates and persists a new key, returning its address.
func (w *Wallet) NewAddress() (string, error) {
	k, err := core.NewKey()
	if err != nil {
		return "", err
	}
	w.keys = append(w.keys, k)
	if err := w.save(); err != nil {
		return "", err
	}
	return core.EncodeAddress(core.PubKeyHash20(k.Public().(ed25519.PublicKey))), nil
}

// Addresses lists all wallet addresses.
func (w *Wallet) Addresses() []string {
	var out []string
	for _, k := range w.keys {
		out = append(out, core.EncodeAddress(core.PubKeyHash20(k.Public().(ed25519.PublicKey))))
	}
	return out
}

// PKHs returns the pubkey hashes of all keys.
func (w *Wallet) PKHs() [][20]byte {
	var out [][20]byte
	for _, k := range w.keys {
		out = append(out, core.PubKeyHash20(k.Public().(ed25519.PublicKey)))
	}
	return out
}

// PrimaryPKH is the default mining/receive key.
func (w *Wallet) PrimaryPKH() [20]byte { return w.PKHs()[0] }

// Balance sums spendable UTXOs across all keys.
func (w *Wallet) Balance(c *core.Chain) int64 {
	var total int64
	for _, pkh := range w.PKHs() {
		for _, e := range c.UTXOsForPKH(pkh) {
			total += e.Value
		}
	}
	return total
}

// Send builds, signs and submits a payment of `amount` units to `toAddr`,
// paying `fee` units, returning the signed transaction.
func (w *Wallet) Send(c *core.Chain, toAddr string, amount, fee int64) (*core.Tx, error) {
	if amount <= 0 {
		return nil, errors.New("amount must be positive")
	}
	toPKH, err := core.DecodeAddress(toAddr)
	if err != nil {
		return nil, fmt.Errorf("bad address: %w", err)
	}
	type cand struct {
		op   core.OutPoint
		e    core.UTXOEntry
		kIdx int
	}
	var cands []cand
	for i, pkh := range w.PKHs() {
		for op, e := range c.UTXOsForPKH(pkh) {
			cands = append(cands, cand{op, e, i})
		}
	}
	// deterministic largest-first selection keeps txs small
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].e.Value != cands[j].e.Value {
			return cands[i].e.Value > cands[j].e.Value
		}
		return string(cands[i].op.TxID[:]) < string(cands[j].op.TxID[:])
	})
	var ins []core.TxIn
	var keys []ed25519.PrivateKey
	var inSum int64
	for _, cd := range cands {
		ins = append(ins, core.TxIn{Prev: cd.op})
		keys = append(keys, w.keys[cd.kIdx])
		inSum += cd.e.Value
		if inSum >= amount+fee {
			break
		}
	}
	if inSum < amount+fee {
		return nil, fmt.Errorf("insufficient funds: have %d need %d", inSum, amount+fee)
	}
	outs := []core.TxOut{{Value: amount, PubKeyHash: toPKH}}
	if change := inSum - amount - fee; change > 0 {
		outs = append(outs, core.TxOut{Value: change, PubKeyHash: w.PrimaryPKH()})
	}
	tx := &core.Tx{Version: 1, Ins: ins, Outs: outs}
	if err := tx.Sign(keys); err != nil {
		return nil, err
	}
	if err := c.AcceptTx(tx); err != nil {
		return nil, err
	}
	return tx, nil
}
