//go:build walletedition

package main

import "flag"

func configureAppMiningFlag(*flag.FlagSet, *appOptions) {}

func resolveAppMiningOptions(options *appOptions, _ string) error {
	options.miningURL = ""
	return nil
}
