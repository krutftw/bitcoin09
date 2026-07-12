package pool

import (
	"context"
	"encoding/hex"
	"fmt"
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
