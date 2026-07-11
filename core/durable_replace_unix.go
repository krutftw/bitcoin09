//go:build !windows

package core

import (
	"errors"
	"os"
	"path/filepath"
)

func atomicReplaceStoreFile(from, to string) error {
	return os.Rename(from, to)
}

func finalizeStoreReplace(path string) error {
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
