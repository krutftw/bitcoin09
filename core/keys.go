package core

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"math/big"
)

// Addresses: version byte 0x09 + 20-byte public key hash, base58check encoded.
const addrVersion = 0x09

// PubKeyHash20 is the address-level hash of an Ed25519 public key:
// first 20 bytes of SHA256(SHA256(pubkey)).
func PubKeyHash20(pub ed25519.PublicKey) (h [20]byte) {
	d := SHA256d(pub)
	copy(h[:], d[:20])
	return
}

// NewKey generates a fresh Ed25519 private key.
func NewKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, err
}

const b58alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58Encode(b []byte) string {
	x := new(big.Int).SetBytes(b)
	radix := big.NewInt(58)
	mod := new(big.Int)
	var out []byte
	for x.Sign() > 0 {
		x.DivMod(x, radix, mod)
		out = append(out, b58alphabet[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, '1')
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

func base58Decode(s string) ([]byte, error) {
	x := big.NewInt(0)
	radix := big.NewInt(58)
	for _, c := range s {
		i := bytes.IndexByte([]byte(b58alphabet), byte(c))
		if i < 0 {
			return nil, errors.New("invalid base58 character")
		}
		x.Mul(x, radix)
		x.Add(x, big.NewInt(int64(i)))
	}
	out := x.Bytes()
	for _, c := range s {
		if c != '1' {
			break
		}
		out = append([]byte{0}, out...)
	}
	return out, nil
}

// EncodeAddress renders a pubkey hash as a BTC09 address.
func EncodeAddress(pkh [20]byte) string {
	payload := append([]byte{addrVersion}, pkh[:]...)
	chk := sha256.Sum256(payload)
	chk = sha256.Sum256(chk[:])
	return base58Encode(append(payload, chk[:4]...))
}

// DecodeAddress parses and checksums a BTC09 address.
func DecodeAddress(s string) (pkh [20]byte, err error) {
	raw, err := base58Decode(s)
	if err != nil {
		return pkh, err
	}
	if len(raw) != 25 || raw[0] != addrVersion {
		return pkh, errors.New("bad address length or version")
	}
	payload, chk := raw[:21], raw[21:]
	want := sha256.Sum256(payload)
	want = sha256.Sum256(want[:])
	if !bytes.Equal(chk, want[:4]) {
		return pkh, errors.New("bad address checksum")
	}
	copy(pkh[:], payload[1:])
	return pkh, nil
}
