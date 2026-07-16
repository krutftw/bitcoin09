//go:build !walletedition

package main

import (
	"errors"
	"flag"
	"net"
	"net/url"
	"strings"
)

func configureAppMiningFlag(flags *flag.FlagSet, options *appOptions) {
	flags.StringVar(&options.miningURL, "miner", "", "Open solo mining coordinator URL")
}

func resolveAppMiningOptions(options *appOptions, network string) error {
	if options.miningURL == "" {
		if network == "mainnet" {
			options.miningURL = defaultMainnetMiningEndpoint
		} else {
			options.miningURL = "http://127.0.0.1:9010"
		}
	}
	return validateAppMiningURL(options.miningURL, network != "mainnet")
}

func validateAppMiningURL(value string, allowLoopbackHTTP bool) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("invalid Open solo mining coordinator URL")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme != "http" || !allowLoopbackHTTP {
		return errors.New("Open solo mining coordinator must use HTTPS")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return errors.New("plain HTTP Open solo coordinator must use loopback")
	}
	return nil
}
