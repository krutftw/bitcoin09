//go:build !windows

package core

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func prepareStoreReplacement(path string, temp *os.File) error {
	descriptor, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return temp.Chmod(0600)
	}
	if err != nil {
		return fmt.Errorf("open existing store snapshot safely: %w", err)
	}
	target := os.NewFile(uintptr(descriptor), path)
	if target == nil {
		_ = unix.Close(descriptor)
		return errors.New("open existing store snapshot safely: invalid descriptor")
	}
	defer target.Close()

	info, err := target.Stat()
	if err != nil {
		return fmt.Errorf("inspect existing store snapshot: %w", err)
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("inspect existing store snapshot: unsupported metadata")
	}
	mode := info.Mode().Perm()
	if !info.Mode().IsRegular() || metadata.Nlink != 1 {
		return errors.New("existing store snapshot is not a single-link regular file")
	}
	if metadata.Uid != uint32(os.Geteuid()) {
		return errors.New("existing store snapshot is not owned by the writer")
	}
	if mode != 0600 && mode != 0640 {
		return fmt.Errorf("existing store snapshot mode %04o is unsafe", mode)
	}
	if err := temp.Chown(int(metadata.Uid), int(metadata.Gid)); err != nil {
		return fmt.Errorf("preserve store snapshot ownership: %w", err)
	}
	if err := temp.Chmod(mode); err != nil {
		return fmt.Errorf("preserve store snapshot mode: %w", err)
	}
	return nil
}
