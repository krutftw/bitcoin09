//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package wallet

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

type walletFileLock struct{ file *os.File }

func acquireWalletFileLock(path string) (*walletFileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return &walletFileLock{file: file}, nil
}

func rejectWalletHardLink(path string) error {
	var info unix.Stat_t
	if err := unix.Stat(path, &info); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Nlink != 1 {
		return errors.New("hard-linked wallet files are not supported")
	}
	return nil
}

func (l *walletFileLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
