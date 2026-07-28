package nineinbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 7, 13, 4, 0, 0, 0, time.UTC)

func testLimits() Limits {
	return Limits{
		MaxItemBytes:       32,
		MaxInboxBytes:      48,
		MaxInboxItems:      2,
		MaxInboxes:         8,
		MaxServiceBytes:    64,
		StandardTTL:        7 * 24 * time.Hour,
		PinnedTTL:          30 * 24 * time.Hour,
		MaxPinnedItemBytes: 16,
	}
}

func TestStoreEnforcesServiceInboxCount(t *testing.T) {
	limits := testLimits()
	limits.MaxInboxes = 1
	store, err := OpenStore(t.TempDir(), limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateInbox(CreateInboxInput{WriteTokenHash: tokenHash([]byte("first")), RecoveryTokenHash: tokenHash([]byte("recover-first"))}); err != nil {
		t.Fatalf("first inbox: %v", err)
	}
	if _, err := store.CreateInbox(CreateInboxInput{WriteTokenHash: tokenHash([]byte("second")), RecoveryTokenHash: tokenHash([]byte("recover-second"))}); !errors.Is(err, ErrServiceFull) {
		t.Fatalf("second inbox error = %v, want ErrServiceFull", err)
	}
}

func tokenHash(token []byte) []byte {
	sum := sha256.Sum256(token)
	return sum[:]
}

func openTestStore(t *testing.T, root string) *Store {
	t.Helper()
	store, err := openStore(root, testLimits(), func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return store
}

func createTestInbox(t *testing.T, store *Store, writeToken, recoveryToken []byte) Inbox {
	t.Helper()
	inbox, err := store.CreateInbox(CreateInboxInput{
		WriteTokenHash:    tokenHash(writeToken),
		RecoveryTokenHash: tokenHash(recoveryToken),
	})
	if err != nil {
		t.Fatalf("CreateInbox: %v", err)
	}
	return inbox
}

func TestStoreLifecycleKeepsOnlyCiphertext(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	writeToken := []byte("write-token-with-enough-entropy-for-tests")
	recoveryToken := []byte("recovery-token-with-enough-entropy-tests")
	inbox := createTestInbox(t, store, writeToken, recoveryToken)

	if len(inbox.ID) < 20 {
		t.Fatalf("inbox ID is too short: %q", inbox.ID)
	}
	if inbox.CreatedAt != testNow {
		t.Fatalf("created at = %v, want %v", inbox.CreatedAt, testNow)
	}

	ciphertext := []byte("opaque-encrypted-item")
	item, err := store.Put(inbox.ID, writeToken, PutItemInput{
		Ciphertext: bytes.NewReader(ciphertext),
		Size:       int64(len(ciphertext)),
		Retention:  RetentionStandard,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if item.ExpiresAt != testNow.Add(7*24*time.Hour) {
		t.Fatalf("expiry = %v", item.ExpiresAt)
	}

	headers, err := store.List(inbox.ID, writeToken)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(headers) != 1 || headers[0].ID != item.ID || headers[0].Size != int64(len(ciphertext)) {
		t.Fatalf("unexpected headers: %#v", headers)
	}

	got, err := store.Get(inbox.ID, item.ID, writeToken)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got.Ciphertext, ciphertext) {
		t.Fatalf("ciphertext = %q", got.Ciphertext)
	}

	if err := store.DeleteItem(inbox.ID, item.ID, writeToken); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	if _, err := store.Get(inbox.ID, item.ID, writeToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete error = %v, want ErrNotFound", err)
	}

	if err := store.DeleteInbox(inbox.ID, recoveryToken); err != nil {
		t.Fatalf("DeleteInbox: %v", err)
	}
	if _, err := store.List(inbox.ID, writeToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("List after inbox delete error = %v, want ErrNotFound", err)
	}
}

func TestStoreRejectsWrongTokensAndInvalidHashes(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	writeToken := []byte("correct-write-token")
	recoveryToken := []byte("correct-recovery-token")
	inbox := createTestInbox(t, store, writeToken, recoveryToken)

	if _, err := store.List(inbox.ID, []byte("wrong")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("List wrong token error = %v", err)
	}
	if err := store.DeleteInbox(inbox.ID, []byte("wrong")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("DeleteInbox wrong token error = %v", err)
	}
	if _, err := store.CreateInbox(CreateInboxInput{WriteTokenHash: []byte("short"), RecoveryTokenHash: make([]byte, 32)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("CreateInbox invalid hash error = %v", err)
	}
}

func TestStoreEnforcesItemInboxAndPinnedLimits(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	writeToken := []byte("write")
	inbox := createTestInbox(t, store, writeToken, []byte("recover"))

	put := func(size int, retention Retention) error {
		_, err := store.Put(inbox.ID, writeToken, PutItemInput{
			Ciphertext: bytes.NewReader(bytes.Repeat([]byte{0xA5}, size)),
			Size:       int64(size),
			Retention:  retention,
		})
		return err
	}

	if err := put(33, RetentionStandard); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
	if err := put(17, RetentionPinned); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize pinned error = %v", err)
	}
	if err := put(24, RetentionStandard); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := put(24, RetentionStandard); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if err := put(1, RetentionStandard); !errors.Is(err, ErrInboxFull) {
		t.Fatalf("item count error = %v", err)
	}
}

func TestStoreEnforcesServiceQuotaAcrossInboxes(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	firstToken := []byte("first")
	secondToken := []byte("second")
	first := createTestInbox(t, store, firstToken, []byte("recover-first"))
	second := createTestInbox(t, store, secondToken, []byte("recover-second"))

	for _, tc := range []struct {
		id    string
		token []byte
		size  int
	}{
		{first.ID, firstToken, 32},
		{second.ID, secondToken, 32},
	} {
		if _, err := store.Put(tc.id, tc.token, PutItemInput{Ciphertext: bytes.NewReader(make([]byte, tc.size)), Size: int64(tc.size), Retention: RetentionStandard}); err != nil {
			t.Fatalf("Put %d bytes: %v", tc.size, err)
		}
	}
	if _, err := store.Put(first.ID, firstToken, PutItemInput{Ciphertext: bytes.NewReader([]byte{1}), Size: 1, Retention: RetentionStandard}); !errors.Is(err, ErrServiceFull) {
		t.Fatalf("service quota error = %v", err)
	}
}

func TestStoreConcurrentQuotaReservation(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	store.limits.MaxInboxBytes = 64
	store.limits.MaxInboxItems = 4
	writeToken := []byte("write")
	inbox := createTestInbox(t, store, writeToken, []byte("recover"))

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.Put(inbox.ID, writeToken, PutItemInput{Ciphertext: bytes.NewReader(make([]byte, 40)), Size: 40, Retention: RetentionStandard})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var success, full int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrTooLarge):
			// The per-item test limit is raised only for this concurrency case.
			full++
		case errors.Is(err, ErrInboxFull):
			full++
		default:
			t.Fatalf("unexpected concurrent error: %v", err)
		}
	}
	if success != 0 || full != 2 {
		t.Fatalf("success=%d full=%d", success, full)
	}

	store.limits.MaxItemBytes = 48
	results = make(chan error, 2)
	start = make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.Put(inbox.ID, writeToken, PutItemInput{Ciphertext: bytes.NewReader(make([]byte, 40)), Size: 40, Retention: RetentionStandard})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success, full = 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrInboxFull) {
			full++
		} else {
			t.Fatalf("unexpected concurrent reservation error: %v", err)
		}
	}
	if success != 1 || full != 1 {
		t.Fatalf("success=%d full=%d, want 1 and 1", success, full)
	}
}

func TestStoreRestartRecoveryAndSweep(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	writeToken := []byte("write")
	inbox := createTestInbox(t, store, writeToken, []byte("recover"))
	item, err := store.Put(inbox.ID, writeToken, PutItemInput{Ciphertext: bytes.NewReader([]byte("ciphertext")), Size: 10, Retention: RetentionStandard})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	tmpPath := filepath.Join(root, "inboxes", inbox.ID, "items", ".stale.tmp")
	if err := os.WriteFile(tmpPath, []byte("partial plaintext must never become visible"), 0o600); err != nil {
		t.Fatalf("WriteFile stale temp: %v", err)
	}
	orphanPath := filepath.Join(root, "inboxes", inbox.ID, "items", "orphan.blob")
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o600); err != nil {
		t.Fatalf("WriteFile orphan: %v", err)
	}

	reopened := openTestStore(t, root)
	got, err := reopened.Get(inbox.ID, item.ID, writeToken)
	if err != nil || string(got.Ciphertext) != "ciphertext" {
		t.Fatalf("reopened Get = %#v, %v", got, err)
	}
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale temp still exists: %v", err)
	}
	if _, err := os.Stat(orphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan blob still exists: %v", err)
	}

	if err := reopened.Sweep(testNow.Add(8 * 24 * time.Hour)); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := reopened.Get(inbox.ID, item.ID, writeToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired Get error = %v", err)
	}
}

func TestStoreRemovesExpiredItemsOnRestart(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	writeToken := []byte("write")
	inbox := createTestInbox(t, store, writeToken, []byte("recover"))
	item, err := store.Put(inbox.ID, writeToken, PutItemInput{Ciphertext: bytes.NewReader([]byte("ciphertext")), Size: 10, Retention: RetentionStandard})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	metaPath := filepath.Join(root, "inboxes", inbox.ID, "items", item.ID+".json")
	var meta diskItem
	body, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile metadata: %v", err)
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}
	meta.ExpiresAt = time.Unix(1, 0).UTC()
	body, err = json.Marshal(meta)
	if err != nil {
		t.Fatalf("Marshal metadata: %v", err)
	}
	if err := os.WriteFile(metaPath, body, 0o600); err != nil {
		t.Fatalf("WriteFile metadata: %v", err)
	}

	reopened := openTestStore(t, root)
	if _, err := reopened.Get(inbox.ID, item.ID, writeToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get expired item after restart error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(root, "inboxes", inbox.ID, "items", item.ID+".blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired blob still exists: %v", err)
	}
}

func TestStoreRejectsTrailingMetadataData(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	inbox := createTestInbox(t, store, []byte("write"), []byte("recover"))
	metaPath := filepath.Join(root, "inboxes", inbox.ID, "meta.json")
	file, err := os.OpenFile(metaPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile metadata: %v", err)
	}
	if _, err := file.WriteString(`{"unexpected":true}`); err != nil {
		_ = file.Close()
		t.Fatalf("append metadata: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close metadata: %v", err)
	}
	if _, err := OpenStore(root, testLimits()); err == nil {
		t.Fatal("OpenStore accepted trailing metadata object")
	}
}

func TestStoreRejectsSymlinkInStorageTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Developer Mode or elevation is required on some Windows installations.
	}
	root := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := OpenStore(root, testLimits()); !errors.Is(err, ErrUnsafeStorage) {
		t.Fatalf("OpenStore symlink error = %v, want ErrUnsafeStorage", err)
	}
}
