// Package pool implements the open Bitcoin 09 remote-solo mining protocol.
package pool

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

// Work is a canonical remote-solo mining job. The client may change only the
// nonce in the final eight bytes of HeaderHex.
type Work struct {
	SchemaVersion int       `json:"schema_version"`
	Network       string    `json:"network"`
	JobID         string    `json:"job_id"`
	Height        int64     `json:"height"`
	HeaderHex     string    `json:"header_hex"`
	TargetHex     string    `json:"target_hex"`
	ExpiresAt     time.Time `json:"expires_at"`
	ArgonMemKiB   uint32    `json:"argon_mem_kib"`
	ArgonTime     uint32    `json:"argon_time"`
}

// MineResult records a locally discovered network-winning nonce.
type MineResult struct {
	Found  bool
	Nonce  uint64
	Hashes uint64
}

// MineProgress is a low-frequency snapshot of one local nonce search. Hashrate
// is the session average for this work item, not an estimate from a single
// sample.
type MineProgress struct {
	Hashes   uint64
	Elapsed  time.Duration
	Hashrate float64
	Final    bool
	Found    bool
}

// ParseWork validates and decodes a coordinator-owned work item.
func ParseWork(work Work, params *core.Params) (core.Header, *big.Int, error) {
	var header core.Header
	if params == nil {
		return header, nil, errors.New("nil network params")
	}
	network, err := core.CanonicalNetworkID(params)
	if err != nil || work.Network != network {
		return header, nil, errors.New("work network mismatch")
	}
	if work.SchemaVersion != 1 || work.Height <= 0 {
		return header, nil, errors.New("unsupported work schema")
	}
	jobID, err := hex.DecodeString(work.JobID)
	if err != nil || len(jobID) != 16 {
		return header, nil, errors.New("invalid job id")
	}
	raw, err := hex.DecodeString(work.HeaderHex)
	if err != nil || len(raw) != 88 {
		return header, nil, errors.New("invalid work header")
	}
	header.Version = binary.LittleEndian.Uint32(raw[0:4])
	copy(header.PrevBlock[:], raw[4:36])
	copy(header.MerkleRoot[:], raw[36:68])
	header.Time = int64(binary.LittleEndian.Uint64(raw[68:76]))
	header.Bits = binary.LittleEndian.Uint32(raw[76:80])
	header.Nonce = binary.LittleEndian.Uint64(raw[80:88])
	if header.Nonce != 0 {
		return core.Header{}, nil, errors.New("work header nonce must be zero")
	}
	if work.ArgonMemKiB != params.ArgonMemKiB || work.ArgonTime != params.ArgonTime {
		return core.Header{}, nil, errors.New("work proof-of-work parameters mismatch")
	}
	targetBytes, err := hex.DecodeString(work.TargetHex)
	if err != nil || len(targetBytes) != 32 {
		return core.Header{}, nil, errors.New("invalid work target")
	}
	target := new(big.Int).SetBytes(targetBytes)
	want := core.CompactToTarget(header.Bits)
	if target.Sign() <= 0 || target.Cmp(want) != 0 || target.Cmp(params.MaxTarget()) > 0 {
		return core.Header{}, nil, errors.New("work target mismatch")
	}
	return header, target, nil
}

// MineWork searches the nonce field of one coordinator-issued work item.
func MineWork(ctx context.Context, work Work, params *core.Params, workers int) (MineResult, error) {
	return MineWorkWithProgress(ctx, work, params, workers, 0, nil)
}

// MineWorkWithProgress searches one coordinator-issued work item and reports
// snapshots outside the hashing goroutines. The callback is serialized and is
// never called from the nonce hot loop.
func MineWorkWithProgress(
	ctx context.Context,
	work Work,
	params *core.Params,
	workers int,
	interval time.Duration,
	callback func(MineProgress),
) (MineResult, error) {
	header, target, err := ParseWork(work, params)
	if err != nil {
		return MineResult{}, err
	}
	if !work.ExpiresAt.After(time.Now()) {
		return MineResult{}, errors.New("work expired")
	}
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	mineCtx, cancel := context.WithDeadline(ctx, work.ExpiresAt)
	defer cancel()
	startedAt := time.Now()

	var hashes atomic.Uint64
	progressStop := make(chan struct{})
	progressDone := make(chan struct{})
	if callback != nil {
		if interval <= 0 {
			interval = time.Second
		}
		go func() {
			defer close(progressDone)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case now := <-ticker.C:
					count := hashes.Load()
					if count == 0 {
						continue
					}
					callback(progressSnapshot(count, now.Sub(startedAt), false, false))
				case <-progressStop:
					return
				}
			}
		}()
	} else {
		close(progressDone)
	}
	stopProgress := func(result MineResult) {
		if callback == nil {
			return
		}
		close(progressStop)
		<-progressDone
		callback(progressSnapshot(result.Hashes, time.Since(startedAt), true, result.Found))
	}

	found := make(chan uint64, 1)
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(start uint64) {
			defer wg.Done()
			candidate := header
			step := uint64(workers)
			for nonce := start; ; nonce += step {
				select {
				case <-mineCtx.Done():
					return
				default:
				}
				candidate.Nonce = nonce
				hashes.Add(1)
				if core.HashToBig(core.PowHash(candidate.Bytes(), params)).Cmp(target) <= 0 {
					select {
					case found <- nonce:
						cancel()
					default:
					}
					return
				}
				if nonce > ^uint64(0)-step {
					return
				}
			}
		}(uint64(worker))
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	<-done
	select {
	case nonce := <-found:
		result := MineResult{Found: true, Nonce: nonce, Hashes: hashes.Load()}
		stopProgress(result)
		return result, nil
	default:
		result := MineResult{Hashes: hashes.Load()}
		stopProgress(result)
		return result, nil
	}
}

func progressSnapshot(hashes uint64, elapsed time.Duration, final, found bool) MineProgress {
	seconds := elapsed.Seconds()
	var hashrate float64
	if seconds > 0 {
		hashrate = float64(hashes) / seconds
	}
	return MineProgress{Hashes: hashes, Elapsed: elapsed, Hashrate: hashrate, Final: final, Found: found}
}
