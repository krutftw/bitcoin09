//go:build walletedition

package main

type appMinerState struct{}

func initializeAppMiner(*appService, appServiceConfig) {}

func appMiningAvailable() bool { return false }

func (*appService) closeAppMiner() {}
