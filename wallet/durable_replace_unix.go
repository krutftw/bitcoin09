//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package wallet

import (
	"errors"
	"os"
	"path/filepath"
)

func durableReplaceWalletFile(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".wallet-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	walletDurabilityStage("temp_created")
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	walletDurabilityStage("file_synced")
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	keep = true
	walletDurabilityStage("renamed")
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	err = errors.Join(syncErr, closeErr)
	if err == nil {
		walletDurabilityStage("directory_synced")
	}
	return err
}
