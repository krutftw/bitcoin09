package nineinbox

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalid       = errors.New("invalid inbox input")
	ErrUnauthorized  = errors.New("inbox authorization failed")
	ErrNotFound      = errors.New("inbox item not found")
	ErrExpired       = errors.New("inbox item expired")
	ErrTooLarge      = errors.New("inbox item too large")
	ErrInboxFull     = errors.New("inbox is full")
	ErrServiceFull   = errors.New("inbox service is full")
	ErrUnsafeStorage = errors.New("unsafe inbox storage")
)

type Retention string

const (
	RetentionStandard Retention = "standard"
	RetentionPinned   Retention = "pinned"
)

type Limits struct {
	MaxItemBytes       int64
	MaxInboxBytes      int64
	MaxInboxItems      int
	MaxServiceBytes    int64
	StandardTTL        time.Duration
	PinnedTTL          time.Duration
	MaxPinnedItemBytes int64
}

type CreateInboxInput struct {
	WriteTokenHash    []byte
	RecoveryTokenHash []byte
}

type Inbox struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Bytes     int64     `json:"bytes"`
	Items     int       `json:"items"`
}

type PutItemInput struct {
	Ciphertext io.Reader
	Size       int64
	Retention  Retention
}

type ItemHeader struct {
	ID        string    `json:"id"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Retention Retention `json:"retention"`
}

type Item struct {
	ItemHeader
	Ciphertext []byte
}

type diskInbox struct {
	ID                string    `json:"id"`
	WriteTokenHash    []byte    `json:"write_token_hash"`
	RecoveryTokenHash []byte    `json:"recovery_token_hash"`
	CreatedAt         time.Time `json:"created_at"`
}

type diskItem struct {
	ID        string    `json:"id"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Retention Retention `json:"retention"`
}

type inboxState struct {
	meta  diskInbox
	items map[string]diskItem
	bytes int64
}

type Store struct {
	mu         sync.Mutex
	root       string
	limits     Limits
	now        func() time.Time
	inboxes    map[string]*inboxState
	totalBytes int64
}

func OpenStore(root string, limits Limits) (*Store, error) {
	if strings.TrimSpace(root) == "" || !validLimits(limits) {
		return nil, ErrInvalid
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve inbox root: %w", err)
	}
	if err := os.MkdirAll(absRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create inbox root: %w", err)
	}
	if err := rejectSymlinks(absRoot); err != nil {
		return nil, err
	}
	inboxesDir := filepath.Join(absRoot, "inboxes")
	if err := os.MkdirAll(inboxesDir, 0o700); err != nil {
		return nil, fmt.Errorf("create inbox directory: %w", err)
	}

	store := &Store{
		root:    absRoot,
		limits:  limits,
		now:     time.Now,
		inboxes: make(map[string]*inboxState),
	}
	if err := store.reconcile(); err != nil {
		return nil, err
	}
	return store, nil
}

func validLimits(l Limits) bool {
	return l.MaxItemBytes > 0 &&
		l.MaxInboxBytes >= l.MaxItemBytes &&
		l.MaxInboxItems > 0 &&
		l.MaxServiceBytes >= l.MaxInboxBytes &&
		l.StandardTTL > 0 &&
		l.PinnedTTL >= l.StandardTTL &&
		l.MaxPinnedItemBytes > 0 &&
		l.MaxPinnedItemBytes <= l.MaxItemBytes
}

func (s *Store) CreateInbox(input CreateInboxInput) (Inbox, error) {
	if len(input.WriteTokenHash) != sha256.Size || len(input.RecoveryTokenHash) != sha256.Size {
		return Inbox{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for attempts := 0; attempts < 8; attempts++ {
		id, err := randomID(16)
		if err != nil {
			return Inbox{}, fmt.Errorf("generate inbox id: %w", err)
		}
		if _, exists := s.inboxes[id]; exists {
			continue
		}
		createdAt := s.now().UTC()
		meta := diskInbox{
			ID:                id,
			WriteTokenHash:    append([]byte(nil), input.WriteTokenHash...),
			RecoveryTokenHash: append([]byte(nil), input.RecoveryTokenHash...),
			CreatedAt:         createdAt,
		}
		dir := s.inboxDir(id)
		if err := os.MkdirAll(filepath.Join(dir, "items"), 0o700); err != nil {
			return Inbox{}, fmt.Errorf("create inbox items directory: %w", err)
		}
		if err := writeJSONAtomic(filepath.Join(dir, "meta.json"), meta); err != nil {
			_ = os.RemoveAll(dir)
			return Inbox{}, fmt.Errorf("write inbox metadata: %w", err)
		}
		s.inboxes[id] = &inboxState{meta: meta, items: make(map[string]diskItem)}
		return Inbox{ID: id, CreatedAt: createdAt}, nil
	}
	return Inbox{}, errors.New("generate unique inbox id")
}

func (s *Store) List(id string, writeToken []byte) ([]ItemHeader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.authorizeWrite(id, writeToken)
	if err != nil {
		return nil, err
	}
	now := s.now()
	items := make([]ItemHeader, 0, len(state.items))
	for _, item := range state.items {
		if !item.ExpiresAt.After(now) {
			continue
		}
		items = append(items, headerFromDisk(item))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID > items[j].ID
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	return items, nil
}

func (s *Store) Put(id string, writeToken []byte, input PutItemInput) (ItemHeader, error) {
	if input.Ciphertext == nil || input.Size < 0 {
		return ItemHeader{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.authorizeWrite(id, writeToken)
	if err != nil {
		return ItemHeader{}, err
	}
	if input.Size > s.limits.MaxItemBytes {
		return ItemHeader{}, ErrTooLarge
	}
	var ttl time.Duration
	switch input.Retention {
	case RetentionStandard:
		ttl = s.limits.StandardTTL
	case RetentionPinned:
		if input.Size > s.limits.MaxPinnedItemBytes {
			return ItemHeader{}, ErrTooLarge
		}
		ttl = s.limits.PinnedTTL
	default:
		return ItemHeader{}, ErrInvalid
	}
	if len(state.items) >= s.limits.MaxInboxItems || state.bytes+input.Size > s.limits.MaxInboxBytes {
		return ItemHeader{}, ErrInboxFull
	}
	if s.totalBytes+input.Size > s.limits.MaxServiceBytes {
		return ItemHeader{}, ErrServiceFull
	}

	itemID, err := s.uniqueItemID(state)
	if err != nil {
		return ItemHeader{}, err
	}
	createdAt := s.now().UTC()
	item := diskItem{
		ID:        itemID,
		Size:      input.Size,
		CreatedAt: createdAt,
		ExpiresAt: createdAt.Add(ttl),
		Retention: input.Retention,
	}
	itemsDir := filepath.Join(s.inboxDir(id), "items")
	tmpPath := filepath.Join(itemsDir, "."+itemID+".tmp")
	blobPath := filepath.Join(itemsDir, itemID+".blob")
	metaPath := filepath.Join(itemsDir, itemID+".json")
	if err := writeExact(tmpPath, input.Ciphertext, input.Size); err != nil {
		_ = os.Remove(tmpPath)
		return ItemHeader{}, err
	}
	if err := os.Rename(tmpPath, blobPath); err != nil {
		_ = os.Remove(tmpPath)
		return ItemHeader{}, fmt.Errorf("publish inbox item: %w", err)
	}
	if err := writeJSONAtomic(metaPath, item); err != nil {
		_ = os.Remove(blobPath)
		return ItemHeader{}, fmt.Errorf("write item metadata: %w", err)
	}
	state.items[itemID] = item
	state.bytes += input.Size
	s.totalBytes += input.Size
	return headerFromDisk(item), nil
}

func (s *Store) Get(id, itemID string, writeToken []byte) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.authorizeWrite(id, writeToken)
	if err != nil {
		return Item{}, err
	}
	item, exists := state.items[itemID]
	if !exists {
		return Item{}, ErrNotFound
	}
	if !item.ExpiresAt.After(s.now()) {
		return Item{}, ErrExpired
	}
	body, err := os.ReadFile(filepath.Join(s.inboxDir(id), "items", itemID+".blob"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Item{}, ErrNotFound
		}
		return Item{}, fmt.Errorf("read inbox item: %w", err)
	}
	if int64(len(body)) != item.Size {
		return Item{}, errors.New("inbox item size mismatch")
	}
	return Item{ItemHeader: headerFromDisk(item), Ciphertext: body}, nil
}

func (s *Store) DeleteItem(id, itemID string, writeToken []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.authorizeWrite(id, writeToken)
	if err != nil {
		return err
	}
	item, exists := state.items[itemID]
	if !exists {
		return ErrNotFound
	}
	if err := removeItemFiles(s.inboxDir(id), itemID); err != nil {
		return err
	}
	delete(state.items, itemID)
	state.bytes -= item.Size
	s.totalBytes -= item.Size
	return nil
}

func (s *Store) DeleteInbox(id string, recoveryToken []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, exists := s.inboxes[id]
	if !exists {
		return ErrNotFound
	}
	if !matchesTokenHash(recoveryToken, state.meta.RecoveryTokenHash) {
		return ErrUnauthorized
	}
	if err := os.RemoveAll(s.inboxDir(id)); err != nil {
		return fmt.Errorf("delete inbox: %w", err)
	}
	s.totalBytes -= state.bytes
	delete(s.inboxes, id)
	return nil
}

func (s *Store) Sweep(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for inboxID, state := range s.inboxes {
		for itemID, item := range state.items {
			if item.ExpiresAt.After(now) {
				continue
			}
			if err := removeItemFiles(s.inboxDir(inboxID), itemID); err != nil {
				return err
			}
			delete(state.items, itemID)
			state.bytes -= item.Size
			s.totalBytes -= item.Size
		}
	}
	return nil
}

func (s *Store) authorizeWrite(id string, token []byte) (*inboxState, error) {
	if !validPublicID(id) {
		return nil, ErrNotFound
	}
	state, exists := s.inboxes[id]
	if !exists {
		return nil, ErrNotFound
	}
	if !matchesTokenHash(token, state.meta.WriteTokenHash) {
		return nil, ErrUnauthorized
	}
	return state, nil
}

func matchesTokenHash(token, expected []byte) bool {
	digest := sha256.Sum256(token)
	return len(expected) == sha256.Size && subtle.ConstantTimeCompare(digest[:], expected) == 1
}

func (s *Store) uniqueItemID(state *inboxState) (string, error) {
	for attempts := 0; attempts < 8; attempts++ {
		id, err := randomID(16)
		if err != nil {
			return "", fmt.Errorf("generate item id: %w", err)
		}
		if _, exists := state.items[id]; !exists {
			return id, nil
		}
	}
	return "", errors.New("generate unique item id")
}

func randomID(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func validPublicID(value string) bool {
	if len(value) < 20 || len(value) > 64 || strings.ContainsAny(value, `/\\.`) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil
}

func headerFromDisk(item diskItem) ItemHeader {
	return ItemHeader{
		ID:        item.ID,
		Size:      item.Size,
		CreatedAt: item.CreatedAt,
		ExpiresAt: item.ExpiresAt,
		Retention: item.Retention,
	}
}

func (s *Store) inboxDir(id string) string {
	return filepath.Join(s.root, "inboxes", id)
}

func writeExact(path string, reader io.Reader, size int64) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create item temp file: %w", err)
	}
	limited := io.LimitReader(reader, size+1)
	written, copyErr := io.Copy(file, limited)
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("write item temp file: %w", copyErr)
	}
	if written != size {
		return ErrInvalid
	}
	if syncErr != nil {
		return fmt.Errorf("sync item temp file: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close item temp file: %w", closeErr)
	}
	return nil
}

func writeJSONAtomic(path string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmpID, err := randomID(8)
	if err != nil {
		return err
	}
	tmpPath := filepath.Join(filepath.Dir(path), "."+tmpID+".tmp")
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	removeTemp := true
	defer func() {
		_ = file.Close()
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	removeTemp = false
	return nil
}

func removeItemFiles(inboxDir, itemID string) error {
	itemsDir := filepath.Join(inboxDir, "items")
	for _, suffix := range []string{".json", ".blob"} {
		if err := os.Remove(filepath.Join(itemsDir, itemID+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete inbox item: %w", err)
		}
	}
	return nil
}

func rejectSymlinks(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrUnsafeStorage, path)
		}
		return nil
	})
}

func (s *Store) reconcile() error {
	if err := rejectSymlinks(s.root); err != nil {
		return err
	}
	inboxesDir := filepath.Join(s.root, "inboxes")
	entries, err := os.ReadDir(inboxesDir)
	if err != nil {
		return fmt.Errorf("read inbox directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validPublicID(entry.Name()) {
			continue
		}
		if err := s.loadInbox(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) loadInbox(id string) error {
	dir := s.inboxDir(id)
	var meta diskInbox
	if err := readJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		return fmt.Errorf("read inbox %s metadata: %w", id, err)
	}
	if meta.ID != id || len(meta.WriteTokenHash) != sha256.Size || len(meta.RecoveryTokenHash) != sha256.Size {
		return fmt.Errorf("%w: invalid inbox metadata", ErrUnsafeStorage)
	}
	itemsDir := filepath.Join(dir, "items")
	if err := os.MkdirAll(itemsDir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(itemsDir)
	if err != nil {
		return err
	}
	state := &inboxState{meta: meta, items: make(map[string]diskItem)}
	knownBlobs := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(itemsDir, name)
		if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp") {
			if err := os.Remove(path); err != nil {
				return err
			}
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		var item diskItem
		if err := readJSON(path, &item); err != nil {
			return fmt.Errorf("read item metadata: %w", err)
		}
		itemID := strings.TrimSuffix(name, ".json")
		if item.ID != itemID || !validPublicID(item.ID) || item.Size < 0 || item.Size > s.limits.MaxItemBytes {
			return fmt.Errorf("%w: invalid item metadata", ErrUnsafeStorage)
		}
		if !item.ExpiresAt.After(s.now()) {
			if err := removeItemFiles(dir, itemID); err != nil {
				return err
			}
			continue
		}
		blobPath := filepath.Join(itemsDir, itemID+".blob")
		info, err := os.Lstat(blobPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				_ = os.Remove(path)
				continue
			}
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != item.Size {
			return fmt.Errorf("%w: invalid item blob", ErrUnsafeStorage)
		}
		state.items[itemID] = item
		state.bytes += item.Size
		knownBlobs[itemID] = true
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".blob") {
			continue
		}
		itemID := strings.TrimSuffix(entry.Name(), ".blob")
		if !knownBlobs[itemID] {
			if err := os.Remove(filepath.Join(itemsDir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if len(state.items) > s.limits.MaxInboxItems || state.bytes > s.limits.MaxInboxBytes || s.totalBytes+state.bytes > s.limits.MaxServiceBytes {
		return fmt.Errorf("%w: stored quota exceeds configured limits", ErrUnsafeStorage)
	}
	s.inboxes[id] = state
	s.totalBytes += state.bytes
	return nil
}

func readJSON(path string, target any) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return ErrInvalid
		}
		return err
	}
	return nil
}
