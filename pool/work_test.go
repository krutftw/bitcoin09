package pool

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

func regtestWork(t *testing.T) Work {
	t.Helper()
	params := core.RegTest
	chain, err := core.NewChain(&params)
	if err != nil {
		t.Fatal(err)
	}
	block := core.BuildBlockTemplate(chain, [20]byte{1}, "open-mining-test")
	target := core.CompactToTarget(block.Header.Bits)
	return Work{
		SchemaVersion: 1,
		Network:       core.RegTestMachineID,
		JobID:         "00112233445566778899aabbccddeeff",
		Height:        1,
		HeaderHex:     hex.EncodeToString(block.Header.Bytes()),
		TargetHex:     fmt.Sprintf("%064x", target),
		ExpiresAt:     time.Now().Add(time.Minute).UTC(),
		ArgonMemKiB:   params.ArgonMemKiB,
		ArgonTime:     params.ArgonTime,
	}
}

func hardRegtestWork(t *testing.T) Work {
	t.Helper()
	work := regtestWork(t)
	raw, err := hex.DecodeString(work.HeaderHex)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(raw[76:80], core.MainNet.MaxTargetBits)
	work.HeaderHex = hex.EncodeToString(raw)
	work.TargetHex = fmt.Sprintf("%064x", core.CompactToTarget(core.MainNet.MaxTargetBits))
	work.ExpiresAt = time.Now().Add(120 * time.Millisecond).UTC()
	return work
}

func TestParseWorkAcceptsCanonicalHeader(t *testing.T) {
	params := core.RegTest
	work := regtestWork(t)
	header, target, err := ParseWork(work, &params)
	if err != nil {
		t.Fatal(err)
	}
	if len(header.Bytes()) != 88 || header.Nonce != 0 {
		t.Fatalf("parsed header = %+v", header)
	}
	if got := fmt.Sprintf("%064x", target); got != work.TargetHex {
		t.Fatalf("target = %s, want %s", got, work.TargetHex)
	}
}

func TestParseWorkRejectsWrongHeaderLength(t *testing.T) {
	params := core.RegTest
	work := regtestWork(t)
	work.HeaderHex = work.HeaderHex[:len(work.HeaderHex)-2]
	if _, _, err := ParseWork(work, &params); err == nil {
		t.Fatal("short header accepted")
	}
}

func TestParseWorkRejectsWrongNetwork(t *testing.T) {
	params := core.RegTest
	work := regtestWork(t)
	work.Network = core.MainNetMachineID
	if _, _, err := ParseWork(work, &params); err == nil {
		t.Fatal("cross-network work accepted")
	}
}

func TestParseWorkRejectsTargetMismatch(t *testing.T) {
	params := core.RegTest
	work := regtestWork(t)
	work.TargetHex = fmt.Sprintf("%064x", core.CompactToTarget(core.MainNet.MaxTargetBits))
	if _, _, err := ParseWork(work, &params); err == nil {
		t.Fatal("server target inconsistent with header bits was accepted")
	}
}

func TestMineWorkFindsRegtestNonce(t *testing.T) {
	params := core.RegTest
	work := regtestWork(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := MineWork(ctx, work, &params, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Found || result.Hashes == 0 {
		t.Fatalf("result = %+v", result)
	}
	header, target, err := ParseWork(work, &params)
	if err != nil {
		t.Fatal(err)
	}
	header.Nonce = result.Nonce
	if core.HashToBig(core.PowHash(header.Bytes(), &params)).Cmp(target) > 0 {
		t.Fatalf("nonce %d does not satisfy target", result.Nonce)
	}
}

func TestMineWorkWithProgressReportsMonotonicHashesAndFinalState(t *testing.T) {
	params := core.RegTest
	work := hardRegtestWork(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var updates []MineProgress
	result, err := MineWorkWithProgress(ctx, work, &params, 2, 10*time.Millisecond, func(progress MineProgress) {
		updates = append(updates, progress)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Found {
		t.Fatal("hard test work unexpectedly found a block")
	}
	if len(updates) < 2 {
		t.Fatalf("progress updates = %d, want periodic plus final", len(updates))
	}
	for index, update := range updates {
		if update.Hashes == 0 || update.Elapsed <= 0 || update.Hashrate <= 0 || math.IsNaN(update.Hashrate) || math.IsInf(update.Hashrate, 0) {
			t.Fatalf("update %d = %+v", index, update)
		}
		if index > 0 && update.Hashes < updates[index-1].Hashes {
			t.Fatalf("hashes decreased: %d then %d", updates[index-1].Hashes, update.Hashes)
		}
		if index < len(updates)-1 && update.Final {
			t.Fatalf("non-final update %d marked final", index)
		}
	}
	final := updates[len(updates)-1]
	if !final.Final || final.Found || final.Hashes != result.Hashes {
		t.Fatalf("final=%+v result=%+v", final, result)
	}
}

func TestMineWorkWithProgressFinalUpdateMarksFoundBlock(t *testing.T) {
	params := core.RegTest
	work := regtestWork(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var final MineProgress
	result, err := MineWorkWithProgress(ctx, work, &params, 1, time.Hour, func(progress MineProgress) {
		if progress.Final {
			final = progress
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Found || !final.Final || !final.Found || final.Hashes != result.Hashes {
		t.Fatalf("final=%+v result=%+v", final, result)
	}
}

func TestMineWorkWithProgressAcceptsNilCallback(t *testing.T) {
	params := core.RegTest
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := MineWorkWithProgress(ctx, regtestWork(t), &params, 1, time.Millisecond, nil)
	if err != nil || !result.Found {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
