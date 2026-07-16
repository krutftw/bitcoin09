package core

import (
	"crypto/sha256"
	"encoding/binary"
	"math/big"

	"golang.org/x/crypto/argon2"
)

// Hash32 is a 32-byte hash.
type Hash32 [32]byte

// SHA256d is Bitcoin's double SHA-256, used for identifiers (block ids,
// txids, merkle nodes). Cheap and collision resistant.
func SHA256d(b []byte) Hash32 {
	h1 := sha256.Sum256(b)
	return sha256.Sum256(h1[:])
}

var powSalt = []byte("BTC09/pow/v1")

// PowHash is the mining hash: Argon2id over the 88-byte block header.
// The 64 MiB memory cost per attempt is what makes mining memory-bound.
func PowHash(header []byte, p *Params) Hash32 {
	var out Hash32
	key := argon2.IDKey(header, powSalt, p.ArgonTime, p.ArgonMemKiB, 1, 32)
	copy(out[:], key)
	return out
}

// HashToBig interprets a hash as a big-endian integer for target comparison.
func HashToBig(h Hash32) *big.Int { return new(big.Int).SetBytes(h[:]) }

// CompactToTarget expands Bitcoin-style compact difficulty bits.
func CompactToTarget(bits uint32) *big.Int {
	size := bits >> 24
	mant := int64(bits & 0x007fffff)
	t := big.NewInt(mant)
	if size <= 3 {
		t.Rsh(t, uint(8*(3-size)))
	} else {
		t.Lsh(t, uint(8*(size-3)))
	}
	return t
}

// TargetToCompact compresses a target into compact bits (canonical form).
func TargetToCompact(t *big.Int) uint32 {
	if t.Sign() <= 0 {
		return 0
	}
	b := t.Bytes()
	size := uint32(len(b))
	var mant uint32
	if size <= 3 {
		mant = uint32(new(big.Int).Set(t).Uint64()) << (8 * (3 - size))
	} else {
		tmp := new(big.Int).Rsh(t, uint(8*(size-3)))
		mant = uint32(tmp.Uint64())
	}
	if mant&0x00800000 != 0 {
		mant >>= 8
		size++
	}
	return size<<24 | (mant & 0x007fffff)
}

// WorkFromTarget returns the expected number of hashes to meet the target:
// 2^256 / (target + 1). Cumulative work decides the best chain.
func WorkFromTarget(target *big.Int) *big.Int {
	num := new(big.Int).Lsh(big.NewInt(1), 256)
	den := new(big.Int).Add(target, big.NewInt(1))
	return num.Div(num, den)
}

// calculateASERTTarget implements the published aserti3 fixed-point
// approximation. All arithmetic that can exceed 64 bits is deliberately done
// with big.Int so every supported platform reaches the same consensus target.
func calculateASERTTarget(
	refTarget *big.Int,
	targetSpacing, timeDiff, heightDiff, halfLife int64,
	maxTarget *big.Int,
) *big.Int {
	one := big.NewInt(1)
	if refTarget == nil || refTarget.Sign() <= 0 {
		return new(big.Int).Set(one)
	}
	if maxTarget == nil || maxTarget.Sign() <= 0 {
		return new(big.Int).Set(one)
	}
	if halfLife <= 0 || targetSpacing <= 0 || heightDiff < 0 {
		if refTarget.Cmp(maxTarget) > 0 {
			return new(big.Int).Set(maxTarget)
		}
		return new(big.Int).Set(refTarget)
	}

	// exponent = ((timeDiff - spacing*(heightDiff+1)) * 65536) / halfLife.
	// Quo truncates toward zero, matching the reference C++ integer division.
	scheduled := new(big.Int).Add(big.NewInt(heightDiff), one)
	scheduled.Mul(scheduled, big.NewInt(targetSpacing))
	exponentBig := new(big.Int).Sub(big.NewInt(timeDiff), scheduled)
	exponentBig.Mul(exponentBig, big.NewInt(65536))
	exponentBig.Quo(exponentBig, big.NewInt(halfLife))
	if !exponentBig.IsInt64() {
		if exponentBig.Sign() < 0 {
			return new(big.Int).Set(one)
		}
		return new(big.Int).Set(maxTarget)
	}
	exponent := exponentBig.Int64()

	// Split into a floored integer component and a non-negative fraction.
	shifts := exponent / 65536
	frac := exponent % 65536
	if frac < 0 {
		shifts--
		frac += 65536
	}

	// 65536 * 2^x, 0 <= x < 1. The maximum intermediate is wider than a
	// signed int64, so retain exact big.Int arithmetic for the polynomial.
	fracBig := big.NewInt(frac)
	factorNumerator := new(big.Int).Mul(big.NewInt(195766423245049), fracBig)
	fracSquared := new(big.Int).Mul(new(big.Int).Set(fracBig), fracBig)
	factorNumerator.Add(factorNumerator,
		new(big.Int).Mul(big.NewInt(971821376), fracSquared))
	fracCubed := new(big.Int).Mul(new(big.Int).Set(fracSquared), fracBig)
	factorNumerator.Add(factorNumerator,
		new(big.Int).Mul(big.NewInt(5127), fracCubed))
	factorNumerator.Add(factorNumerator, new(big.Int).Lsh(one, 47))
	factorNumerator.Rsh(factorNumerator, 48)
	factor := factorNumerator.Add(factorNumerator, big.NewInt(65536))

	nextTarget := new(big.Int).Mul(new(big.Int).Set(refTarget), factor)
	shifts -= 16 // divide the fixed-point factor by 65536
	if shifts <= 0 {
		right := -shifts
		if right >= int64(nextTarget.BitLen()) {
			nextTarget.SetInt64(0)
		} else {
			nextTarget.Rsh(nextTarget, uint(right))
		}
	} else {
		if shifts > int64(maxTarget.BitLen()+17) {
			return new(big.Int).Set(maxTarget)
		}
		nextTarget.Lsh(nextTarget, uint(shifts))
	}

	if nextTarget.Sign() == 0 {
		return new(big.Int).Set(one)
	}
	if nextTarget.Cmp(maxTarget) > 0 {
		return new(big.Int).Set(maxTarget)
	}
	return nextTarget
}

func asertActive(p *Params, height int64) bool {
	return p != nil && p.ASERTActivationHeight > 0 && height >= p.ASERTActivationHeight
}

// NextBits returns the required difficulty bits for the block at `height`,
// given accessors for ancestor headers on the same chain.
//
// Retarget every RetargetInterval blocks by actual/expected timespan,
// clamped to 4x per adjustment (Bitcoin-classic behaviour, shorter window).
func NextBits(p *Params, height int64, bitsAt func(h int64) uint32, timeAt func(h int64) int64) uint32 {
	if height == 0 {
		return p.MaxTargetBits
	}
	if asertActive(p, height) {
		anchorHeight := p.ASERTActivationHeight - 1
		parentHeight := height - 1
		target := calculateASERTTarget(
			CompactToTarget(bitsAt(anchorHeight)),
			p.TargetBlockTime,
			timeAt(parentHeight)-timeAt(anchorHeight-1),
			parentHeight-anchorHeight,
			p.ASERTHalfLife,
			p.MaxTarget(),
		)
		return TargetToCompact(target)
	}
	prev := height - 1
	if height%p.RetargetInterval != 0 {
		return bitsAt(prev)
	}
	first := height - p.RetargetInterval
	actual := timeAt(prev) - timeAt(first)
	expected := p.TargetBlockTime * (p.RetargetInterval - 1)
	if expected <= 0 {
		expected = 1
	}
	if actual < expected/4 {
		actual = expected / 4
	}
	if actual > expected*4 {
		actual = expected * 4
	}
	oldT := CompactToTarget(bitsAt(prev))
	newT := new(big.Int).Mul(oldT, big.NewInt(actual))
	newT.Div(newT, big.NewInt(expected))
	if maxT := p.MaxTarget(); newT.Cmp(maxT) > 0 {
		newT = maxT
	}
	return TargetToCompact(newT)
}

// putUvarint appends a uvarint to dst (serialization helper).
func putUvarint(dst []byte, v uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	return append(dst, tmp[:n]...)
}
