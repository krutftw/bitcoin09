//go:build windows

package core

import "os"

func prepareStoreReplacement(_ string, temp *os.File) error {
	return temp.Chmod(0600)
}
