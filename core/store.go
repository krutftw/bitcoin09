package core

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Store persists main-chain blocks to an append-only file:
// repeated [4-byte BE length][block bytes] records, heights 1..N in order.
// (Genesis is derived from Params, never stored.) On divergence (reorg past
// a stored block) the file is rewritten from the in-memory main chain.
type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(dataDir string, network string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dataDir, "blocks-"+network+".dat")}, nil
}

// LoadInto replays stored blocks into the chain. Corrupt tails truncate.
func (s *Store) LoadInto(c *Chain) (int64, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var loaded int64
	for {
		var lenb [4]byte
		if _, err := io.ReadFull(r, lenb[:]); err != nil {
			break // clean EOF or truncated tail: stop
		}
		ln := binary.BigEndian.Uint32(lenb[:])
		if ln > MaxBlockBytes+16 {
			break
		}
		raw := make([]byte, ln)
		if _, err := io.ReadFull(r, raw); err != nil {
			break
		}
		blk, err := DecodeBlock(raw)
		if err != nil {
			break
		}
		if err := c.acceptStoredBlock(blk); err != nil {
			return loaded, fmt.Errorf("stored block rejected at height ~%d: %w", loaded+1, err)
		}
		loaded++
	}
	return loaded, nil
}

// SaveSnapshot rewrites the file to match the chain's current main chain.
// Called on new tips; cheap at hobby-chain sizes, atomic via temp+rename.
func (s *Store) SaveSnapshot(c *Chain) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp := s.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	_, tipH := c.Tip()
	for h := int64(1); h <= tipH; h++ {
		blk := c.BlockAt(h)
		if blk == nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("missing block at height %d", h)
		}
		raw := blk.Bytes()
		var lenb [4]byte
		binary.BigEndian.PutUint32(lenb[:], uint32(len(raw)))
		if _, err := w.Write(lenb[:]); err != nil {
			f.Close()
			return err
		}
		if _, err := w.Write(raw); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
