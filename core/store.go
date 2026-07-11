package core

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sync"
)

var ErrSnapshotRegression = errors.New("canonical snapshot would regress durable work")

type canonicalSnapshotSource interface {
	canonicalMainSnapshot() (canonicalMainSnapshot, error)
}

type storeFileOps struct {
	createTemp func(string, string) (*os.File, error)
	prepare    func(string, *os.File) error
	write      func(*bufio.Writer, []byte) (int, error)
	flush      func(*bufio.Writer) error
	syncFile   func(*os.File) error
	closeFile  func(*os.File) error
	replace    func(string, string) error
	finalize   func(string) error
	remove     func(string) error
	afterRead  func()
}

func defaultStoreFileOps() storeFileOps {
	return storeFileOps{
		createTemp: os.CreateTemp,
		prepare:    prepareStoreReplacement,
		write: func(writer *bufio.Writer, data []byte) (int, error) {
			return writer.Write(data)
		},
		flush:     func(writer *bufio.Writer) error { return writer.Flush() },
		syncFile:  func(file *os.File) error { return file.Sync() },
		closeFile: func(file *os.File) error { return file.Close() },
		replace:   atomicReplaceStoreFile,
		finalize:  finalizeStoreReplace,
		remove:    os.Remove,
	}
}

// Store persists canonical blocks as repeated [4-byte BE length][block bytes]
// records for heights 1..N. Genesis is derived from the selected network.
// Every reader/writer uses both an in-process mutex and a canonical-path OS
// lock; multiple Store instances and processes therefore share one ordering.
type Store struct {
	path     string
	lockPath string
	network  string
	mu       sync.Mutex
	ops      storeFileOps
}

func NewStore(dataDir string, network string) (*Store, error) {
	if _, ok := canonicalStoreParams(network); !ok {
		return nil, fmt.Errorf("unsupported store network %q", network)
	}
	if dataDir == "" {
		return nil, errors.New("empty store data directory")
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	absDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	absDir = filepath.Clean(absDir)
	canonicalDir, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(canonicalDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("store data path is not a directory")
	}
	path := filepath.Join(canonicalDir, "blocks-"+network+".dat")
	return &Store{
		path:     path,
		lockPath: path + ".lock",
		network:  network,
		ops:      defaultStoreFileOps(),
	}, nil
}

// LoadInto strictly validates the complete durable file on a scratch chain
// while holding the OS lock, then atomically installs that state into c. A
// corrupt/truncated tail never leaves the caller partially advanced.
func (s *Store) LoadInto(c *Chain) (height int64, err error) {
	if c == nil {
		return 0, errors.New("nil destination chain")
	}
	expectedParams, _ := canonicalStoreParams(s.network)
	c.mu.RLock()
	destinationErr := validateLoadDestinationLocked(c, expectedParams)
	c.mu.RUnlock()
	if destinationErr != nil {
		return 0, destinationErr
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := acquireStoreFileLock(s.lockPath)
	if err != nil {
		return 0, err
	}
	defer func() {
		err = errors.Join(err, lock.release())
	}()
	scratch, snapshot, _, exists, readErr := s.readDurableSnapshotLocked(c.Params())
	if s.ops.afterRead != nil {
		s.ops.afterRead()
	}
	if readErr != nil {
		return 0, readErr
	}
	if !exists {
		return 0, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := validateLoadDestinationLocked(c, expectedParams); err != nil {
		return 0, err
	}
	if snapshot.params != expectedParams {
		return 0, errors.New("durable snapshot consensus params mismatch")
	}
	// scratch is private to this call; transferring its maps and index pointers
	// gives c sole ownership without exposing mutable aliases.
	c.index = scratch.index
	c.tip = scratch.tip
	c.mainIDs = scratch.mainIDs
	c.utxo = scratch.utxo
	c.mempool = make(map[Hash32]*Tx)
	return snapshot.tipHeight, nil
}

func canonicalStoreParams(network string) (Params, bool) {
	switch network {
	case MainNet.Name:
		return MainNet, true
	case RegTest.Name:
		return RegTest, true
	default:
		return Params{}, false
	}
}

func validateLoadDestinationLocked(c *Chain, expected Params) error {
	if c.params == nil || *c.params != expected {
		return errors.New("destination chain consensus params do not match Store network")
	}
	fresh, err := NewChain(&expected)
	if err != nil {
		return fmt.Errorf("construct canonical genesis state: %w", err)
	}
	if !matchesCanonicalGenesisLocked(c, fresh) {
		return errors.New("LoadInto destination must be an empty genesis chain")
	}
	return nil
}

func matchesCanonicalGenesisLocked(c, fresh *Chain) bool {
	if c.tip == nil || fresh.tip == nil || len(c.mempool) != 0 ||
		len(c.mainIDs) != len(fresh.mainIDs) ||
		len(c.index) != len(fresh.index) || len(c.utxo) != len(fresh.utxo) {
		return false
	}
	for height, wantID := range fresh.mainIDs {
		if c.mainIDs[height] != wantID {
			return false
		}
	}
	for outpoint, wantEntry := range fresh.utxo {
		if gotEntry, ok := c.utxo[outpoint]; !ok || gotEntry != wantEntry {
			return false
		}
	}
	for id, wantIndex := range fresh.index {
		gotIndex, ok := c.index[id]
		if !ok || gotIndex == nil || gotIndex.id != wantIndex.id ||
			gotIndex.height != wantIndex.height || gotIndex.cumWork == nil ||
			gotIndex.cumWork.Cmp(wantIndex.cumWork) != 0 ||
			!equalBlockBytes(gotIndex.block, wantIndex.block) {
			return false
		}
	}
	return c.tip == c.index[fresh.tip.id] && c.tip.id == fresh.tip.id
}

func equalBlockBytes(got, want *Block) bool {
	if got == nil || want == nil || len(got.Txs) != len(want.Txs) {
		return false
	}
	for i := range got.Txs {
		if got.Txs[i] == nil || want.Txs[i] == nil {
			return false
		}
	}
	return bytes.Equal(got.Bytes(), want.Bytes())
}

// SaveSnapshot serializes one detached canonical Chain snapshot. Lock order is
// Store.mu -> OS file lock -> one Chain RLock snapshot -> disk operations.
func (s *Store) SaveSnapshot(c *Chain) error {
	if c == nil {
		return errors.New("nil source chain")
	}
	return s.saveSnapshot(c)
}

func (s *Store) saveSnapshot(source canonicalSnapshotSource) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	lock, err := acquireStoreFileLock(s.lockPath)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, lock.release())
	}()

	candidate, err := source.canonicalMainSnapshot()
	if err != nil {
		return err
	}
	expectedParams, _ := canonicalStoreParams(s.network)
	if candidate.params != expectedParams {
		return errors.New("candidate snapshot consensus params mismatch")
	}
	candidateBytes, err := validateAndEncodeCanonicalSnapshot(candidate)
	if err != nil {
		return err
	}

	_, current, currentBytes, exists, err := s.readDurableSnapshotLocked(&candidate.params)
	if err != nil {
		return err
	}
	if exists {
		switch candidate.cumWork.Cmp(current.cumWork) {
		case -1:
			return ErrSnapshotRegression
		case 0:
			if candidate.tipID != current.tipID {
				return fmt.Errorf("%w: equal work has different tip", ErrSnapshotRegression)
			}
			if !bytes.Equal(candidateBytes, currentBytes) {
				return errors.New("same durable tip has noncanonical byte mismatch")
			}
			// A prior atomic replacement may have made these exact bytes visible
			// before its final durability step failed. Re-run that completion
			// barrier without creating or replacing another temporary file.
			return s.ops.finalize(s.path)
		}
	}
	return s.durableReplace(candidateBytes)
}

// readDurableSnapshotLocked must only be called while the caller owns the OS
// lock. It never calls public LoadInto, avoiding recursive lock acquisition.
func (s *Store) readDurableSnapshotLocked(params *Params) (
	*Chain,
	canonicalMainSnapshot,
	[]byte,
	bool,
	error,
) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		scratch, chainErr := NewChain(params)
		if chainErr != nil {
			return nil, canonicalMainSnapshot{}, nil, false, chainErr
		}
		snapshot, chainErr := scratch.canonicalMainSnapshot()
		return scratch, snapshot, nil, false, chainErr
	}
	if err != nil {
		return nil, canonicalMainSnapshot{}, nil, false, err
	}
	scratch, snapshot, canonical, err := decodeCanonicalSnapshot(params, data)
	if err != nil {
		return nil, canonicalMainSnapshot{}, nil, true, err
	}
	if !bytes.Equal(data, canonical) {
		return nil, canonicalMainSnapshot{}, nil, true,
			errors.New("durable snapshot is not canonically encoded")
	}
	return scratch, snapshot, canonical, true, nil
}

func decodeCanonicalSnapshot(
	params *Params,
	data []byte,
) (*Chain, canonicalMainSnapshot, []byte, error) {
	scratch, err := NewChain(params)
	if err != nil {
		return nil, canonicalMainSnapshot{}, nil, err
	}
	reader := bytes.NewReader(data)
	var height int64
	for reader.Len() > 0 {
		if reader.Len() < 4 {
			return nil, canonicalMainSnapshot{}, nil, errors.New("truncated snapshot frame length")
		}
		var lengthBytes [4]byte
		if _, err := io.ReadFull(reader, lengthBytes[:]); err != nil {
			return nil, canonicalMainSnapshot{}, nil, err
		}
		length := binary.BigEndian.Uint32(lengthBytes[:])
		if length == 0 || length > MaxBlockBytes {
			return nil, canonicalMainSnapshot{}, nil, errors.New("invalid stored block length")
		}
		if uint64(length) > uint64(reader.Len()) {
			return nil, canonicalMainSnapshot{}, nil, errors.New("truncated stored block")
		}
		raw := make([]byte, int(length))
		if _, err := io.ReadFull(reader, raw); err != nil {
			return nil, canonicalMainSnapshot{}, nil, err
		}
		block, err := DecodeBlock(raw)
		if err != nil {
			return nil, canonicalMainSnapshot{}, nil, fmt.Errorf("decode stored block: %w", err)
		}
		if err := scratch.acceptStoredBlock(block); err != nil {
			return nil, canonicalMainSnapshot{}, nil,
				fmt.Errorf("stored block rejected at height %d: %w", height+1, err)
		}
		height++
		tipID, tipHeight := scratch.Tip()
		if tipHeight != height || tipID != block.Header.ID() {
			return nil, canonicalMainSnapshot{}, nil,
				fmt.Errorf("stored block %d is not the canonical next tip", height)
		}
	}
	snapshot, err := scratch.canonicalMainSnapshot()
	if err != nil {
		return nil, canonicalMainSnapshot{}, nil, err
	}
	canonical, err := encodeCanonicalSnapshot(snapshot)
	if err != nil {
		return nil, canonicalMainSnapshot{}, nil, err
	}
	return scratch, snapshot, canonical, nil
}

func validateAndEncodeCanonicalSnapshot(snapshot canonicalMainSnapshot) ([]byte, error) {
	encoded, err := encodeCanonicalSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	_, validated, canonical, err := decodeCanonicalSnapshot(&snapshot.params, encoded)
	if err != nil {
		return nil, err
	}
	if validated.tipID != snapshot.tipID ||
		validated.tipHeight != snapshot.tipHeight ||
		validated.cumWork.Cmp(snapshot.cumWork) != 0 ||
		!bytes.Equal(canonical, encoded) {
		return nil, errors.New("candidate canonical snapshot metadata mismatch")
	}
	return canonical, nil
}

func encodeCanonicalSnapshot(snapshot canonicalMainSnapshot) ([]byte, error) {
	if snapshot.params.Name != MainNet.Name && snapshot.params.Name != RegTest.Name {
		return nil, errors.New("snapshot has unsupported network")
	}
	if snapshot.tipHeight < 0 || int64(len(snapshot.blocks)) != snapshot.tipHeight {
		return nil, errors.New("snapshot height/block count mismatch")
	}
	if snapshot.cumWork == nil || snapshot.cumWork.Sign() <= 0 {
		return nil, errors.New("snapshot cumulative work is invalid")
	}
	genesis := GenesisBlock(&snapshot.params)
	previousID := genesis.Header.ID()
	expectedWork := WorkFromTarget(CompactToTarget(genesis.Header.Bits))
	var buffer bytes.Buffer
	for index, block := range snapshot.blocks {
		height := int64(index + 1)
		if block == nil || block.Header.PrevBlock != previousID {
			return nil, fmt.Errorf("snapshot block %d parent mismatch", height)
		}
		if block.Header.MerkleRoot != MerkleRoot(block.Txs) {
			return nil, fmt.Errorf("snapshot block %d merkle mismatch", height)
		}
		raw := block.Bytes()
		if len(raw) == 0 || len(raw) > MaxBlockBytes {
			return nil, fmt.Errorf("snapshot block %d size is invalid", height)
		}
		var lengthBytes [4]byte
		binary.BigEndian.PutUint32(lengthBytes[:], uint32(len(raw)))
		buffer.Write(lengthBytes[:])
		buffer.Write(raw)
		previousID = block.Header.ID()
		expectedWork = new(big.Int).Add(expectedWork,
			WorkFromTarget(CompactToTarget(block.Header.Bits)))
	}
	if previousID != snapshot.tipID || expectedWork.Cmp(snapshot.cumWork) != 0 {
		return nil, errors.New("snapshot tip/work metadata mismatch")
	}
	return buffer.Bytes(), nil
}

func (s *Store) durableReplace(data []byte) (err error) {
	directory := filepath.Dir(s.path)
	pattern := "." + filepath.Base(s.path) + ".tmp-*"
	file, err := s.ops.createTemp(directory, pattern)
	if err != nil {
		return err
	}
	tempPath := file.Name()
	closed := false
	replaced := false
	defer func() {
		if !closed {
			err = errors.Join(err, file.Close())
		}
		if !replaced {
			if removeErr := s.ops.remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
	}()

	tempAbs, err := filepath.Abs(tempPath)
	if err != nil {
		return err
	}
	if filepath.Dir(filepath.Clean(tempAbs)) != directory {
		return errors.New("temporary snapshot is outside destination directory")
	}
	writer := bufio.NewWriterSize(file, 1<<20)
	written, err := s.ops.write(writer, data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	if err := s.ops.flush(writer); err != nil {
		return err
	}
	if err := s.ops.prepare(s.path, file); err != nil {
		return err
	}
	if err := s.ops.syncFile(file); err != nil {
		return err
	}
	if err := s.ops.closeFile(file); err != nil {
		return err
	}
	closed = true
	if err := s.ops.replace(tempPath, s.path); err != nil {
		return err
	}
	replaced = true
	if err := s.ops.finalize(s.path); err != nil {
		return err
	}
	return nil
}
