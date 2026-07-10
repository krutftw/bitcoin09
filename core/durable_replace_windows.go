//go:build windows

package core

import "golang.org/x/sys/windows"

func atomicReplaceStoreFile(from, to string) error {
	fromUTF16, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toUTF16, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		fromUTF16,
		toUTF16,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

// MoveFileEx with MOVEFILE_WRITE_THROUGH includes the platform's final
// durability completion; no unsupported secondary primitive is invented.
func finalizeStoreReplace(string) error { return nil }
