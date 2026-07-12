//go:build darwin

package main

import "os/exec"

func openBrowser(target string) error {
	return exec.Command("open", target).Start()
}
