//go:build walletedition

package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/krutftw/bitcoin09/core"
)

// nodeVersion is the release version; bump alongside git tags.
const nodeVersion = "v0.1.33"

func main() {
	log.SetFlags(log.Ltime)
	command, args := selectCommand(os.Args[1:])
	command, args, err := enforceDistributionEdition(command, args, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	switch command {
	case "app":
		cmdApp(args)
	case "version":
		fmt.Printf("%s (%s) reference node %s (wallet edition)\n", core.CoinName, core.Ticker, nodeVersion)
	default:
		panic("wallet distribution command gate failed")
	}
}

func selectCommand(args []string) (string, []string) {
	if len(args) == 0 {
		return "app", nil
	}
	return args[0], args[1:]
}

func enforceDistributionEdition(command string, args []string, walletEdition bool) (string, []string, error) {
	if !walletEdition {
		return command, args, nil
	}
	if command == "version" {
		return command, args, nil
	}
	if command != "app" {
		return "", nil, errors.New("That command is not available in the BTC09 Wallet edition.")
	}

	locked := make([]string, 0, len(args)+1)
	locked = append(locked, "-wallet-only")
	for _, arg := range args {
		if arg == "-wallet-only" || arg == "--wallet-only" ||
			strings.HasPrefix(arg, "-wallet-only=") || strings.HasPrefix(arg, "--wallet-only=") {
			continue
		}
		locked = append(locked, arg)
	}
	return command, locked, nil
}

func defaultDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".btc09")
}

func humanParams(name string) (*core.Params, error) {
	switch name {
	case "mainnet":
		params := core.MainNet
		return &params, nil
	case "regtest":
		params := core.RegTest
		return &params, nil
	default:
		return nil, fmt.Errorf("unknown network %q", name)
	}
}

func defaultSeeds(params *core.Params) []string {
	if params.Name != "mainnet" {
		return nil
	}
	return []string{
		"seed.btc09.org:9009",
		"178.128.52.20:9009",
		"178.128.105.41:9009",
		"103.80.18.140:9009",
		"108.190.240.138:9009",
	}
}

func resolveHumanWalletPath(explicit, environment, dataDir, network string) string {
	if explicit != "" {
		return explicit
	}
	if environment != "" {
		return environment
	}
	return filepath.Join(dataDir, "wallet-"+network+".json")
}
