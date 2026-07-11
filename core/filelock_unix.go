//go:build !windows

package core

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

type storeFileLock struct {
	file *os.File
}

func acquireStoreFileLock(path string) (*storeFileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &storeFileLock{file: file}, nil
}

func (lock *storeFileLock) release() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}
