//go:build !windows && !darwin

package main

import "os/exec"

func openBrowser(target string) error {
	return exec.Command("xdg-open", target).Start()
}
