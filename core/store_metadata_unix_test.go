//go:build !windows

package core

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func unixStoreMetadata(t *testing.T, path string) (os.FileMode, uint32, uint32, uint64) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat %s: %v", path, err)
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("store metadata has an unexpected type")
	}
	return info.Mode().Perm(), metadata.Uid, metadata.Gid, metadata.Ino
}

func TestStoreAtomicReplacementPreservesSafeExistingMetadata(t *testing.T) {
	store, err := NewStore(t.TempDir(), RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	chain := testChain(t)
	_, pkh := keyAndPKH(t)
	mineOne(t, chain, pkh)
	if err := store.SaveSnapshot(chain); err != nil {
		t.Fatalf("first SaveSnapshot: %v", err)
	}
	mode, uid, _, firstInode := unixStoreMetadata(t, store.path)
	if mode != 0600 || uid != uint32(os.Geteuid()) {
		t.Fatalf("first snapshot metadata = %04o uid %d, want 0600 uid %d", mode, uid, os.Geteuid())
	}
	if err := os.Chown(store.path, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatalf("Chown shared snapshot: %v", err)
	}
	if err := os.Chmod(store.path, 0640); err != nil {
		t.Fatalf("Chmod shared snapshot: %v", err)
	}
	mineOne(t, chain, pkh)
	if err := store.SaveSnapshot(chain); err != nil {
		t.Fatalf("replacement SaveSnapshot: %v", err)
	}
	mode, uid, gid, secondInode := unixStoreMetadata(t, store.path)
	if mode != 0640 || uid != uint32(os.Geteuid()) || gid != uint32(os.Getegid()) {
		t.Fatalf("replacement metadata = %04o %d:%d, want 0640 %d:%d", mode, uid, gid, os.Geteuid(), os.Getegid())
	}
	if secondInode == firstInode {
		t.Fatal("snapshot replacement did not install a new inode")
	}
}

func TestStoreAtomicReplacementRejectsUnsafeExistingEntries(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string) string
	}{
		{
			name: "unsafe_mode",
			mutate: func(t *testing.T, path string) string {
				t.Helper()
				if err := os.Chmod(path, 0660); err != nil {
					t.Fatalf("Chmod unsafe snapshot: %v", err)
				}
				return path
			},
		},
		{
			name: "hard_link",
			mutate: func(t *testing.T, path string) string {
				t.Helper()
				if err := os.Link(path, path+".alias"); err != nil {
					t.Fatalf("create hard link: %v", err)
				}
				return path
			},
		},
		{
			name: "symbolic_link",
			mutate: func(t *testing.T, path string) string {
				t.Helper()
				outside := filepath.Join(filepath.Dir(path), "outside-snapshot")
				if err := os.Rename(path, outside); err != nil {
					t.Fatalf("move snapshot outside: %v", err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatalf("create snapshot symlink: %v", err)
				}
				return outside
			},
		},
	}
	if os.Geteuid() == 0 {
		tests = append(tests, struct {
			name   string
			mutate func(*testing.T, string) string
		}{
			name: "wrong_owner",
			mutate: func(t *testing.T, path string) string {
				t.Helper()
				if err := os.Chown(path, 1, os.Getegid()); err != nil {
					t.Fatalf("Chown snapshot to wrong owner: %v", err)
				}
				return path
			},
		})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir(), RegTest.Name)
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			chain := testChain(t)
			_, pkh := keyAndPKH(t)
			mineOne(t, chain, pkh)
			if err := store.SaveSnapshot(chain); err != nil {
				t.Fatalf("initial SaveSnapshot: %v", err)
			}
			preservedPath := test.mutate(t, store.path)
			before, err := os.ReadFile(preservedPath)
			if err != nil {
				t.Fatalf("read preserved snapshot: %v", err)
			}
			mineOne(t, chain, pkh)
			if err := store.SaveSnapshot(chain); err == nil {
				t.Fatal("SaveSnapshot accepted an unsafe existing entry")
			}
			after, err := os.ReadFile(preservedPath)
			if err != nil {
				t.Fatalf("re-read preserved snapshot: %v", err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("unsafe-entry rejection changed the prior snapshot")
			}
		})
	}
}
