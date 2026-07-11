//go:build windows

package wallet

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
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
	if err := windows.FlushFileBuffers(windows.Handle(temp.Fd())); err != nil {
		temp.Close()
		return err
	}
	walletDurabilityStage("file_synced")
	if err := temp.Close(); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tempPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return err
	}
	keep = true
	walletDurabilityStage("renamed")
	// MOVEFILE_WRITE_THROUGH does not return until the replacement has been
	// flushed. Windows does not offer a portable directory-fsync equivalent;
	// the temp file handle itself was explicitly flushed above.
	walletDurabilityStage("directory_synced")
	return nil
}
