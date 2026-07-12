//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

func openBrowser(target string) error {
	command := exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return command.Start()
}
