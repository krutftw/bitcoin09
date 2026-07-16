//go:build walletedition

package main

import (
	"testing"

	"github.com/krutftw/bitcoin09/desktop"
)

func TestWalletEditionDoesNotImplementMiningService(t *testing.T) {
	var service any = (*appService)(nil)
	if _, ok := service.(desktop.MinerService); ok {
		t.Fatal("wallet edition service implements mining")
	}
	if appMiningAvailable() {
		t.Fatal("wallet edition reports mining as available")
	}
}

func TestWalletEditionDoesNotRegisterMiningOptions(t *testing.T) {
	if _, err := parseAppOptions([]string{"-miner", "https://example.com"}); err == nil {
		t.Fatal("wallet edition accepted a mining endpoint")
	}
	options, err := parseAppOptions([]string{"-wallet-only"})
	if err != nil {
		t.Fatal(err)
	}
	if options.miningURL != "" || !options.walletOnly {
		t.Fatalf("wallet edition options = %+v", options)
	}
}
