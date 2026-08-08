package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func persistedApiKey(t *testing.T, path, id string) *ApiKeyEntry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	var persisted Config
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("decode persisted config: %v", err)
	}
	for i := range persisted.ApiKeys {
		if persisted.ApiKeys[i].ID == id {
			entry := persisted.ApiKeys[i]
			return &entry
		}
	}
	return nil
}

func setConfigPathForTest(t *testing.T, path string) func() {
	t.Helper()
	cfgLock.Lock()
	previous := cfgPath
	cfgPath = path
	cfgLock.Unlock()
	return func() {
		cfgLock.Lock()
		cfgPath = previous
		cfgLock.Unlock()
	}
}

func TestRecordApiKeyUsageIsMemoryOnlyAndVisibleToReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{
		Name:        "pending",
		Key:         "sk-pending",
		Enabled:     true,
		TokenLimit:  10,
		CreditLimit: 1,
	})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	// Record must not wait for the global configuration lock. This is the key
	// request-path property that prevents unrelated config persistence from
	// inflating authentication and dispatch latency.
	cfgLock.Lock()
	recorded := make(chan error, 1)
	go func() { recorded <- RecordApiKeyUsage(created.ID, 10, 1.25) }()
	select {
	case err := <-recorded:
		if err != nil {
			t.Fatalf("RecordApiKeyUsage: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		cfgLock.Unlock()
		t.Fatal("RecordApiKeyUsage blocked on cfgLock")
	}
	cfgLock.Unlock()

	for name, entry := range map[string]*ApiKeyEntry{
		"get":  GetApiKeyEntry(created.ID),
		"find": FindApiKeyByValue(created.Key),
	} {
		if entry == nil {
			t.Fatalf("%s reader did not find API key", name)
		}
		if entry.TokensUsed != 10 || entry.CreditsUsed != 1.25 || entry.RequestsCount != 1 || entry.LastUsedAt == 0 {
			t.Fatalf("%s reader did not merge pending usage: %+v", name, entry)
		}
		if overToken, overCredit := ApiKeyOverLimit(*entry); !overToken || !overCredit {
			t.Fatalf("%s quota check missed pending usage: token=%v credit=%v", name, overToken, overCredit)
		}
	}
	listed := ListApiKeys()
	if len(listed) != 1 || listed[0].TokensUsed != 10 || listed[0].RequestsCount != 1 {
		t.Fatalf("ListApiKeys did not merge pending usage: %+v", listed)
	}

	if disk := persistedApiKey(t, path, created.ID); disk == nil || disk.TokensUsed != 0 || disk.RequestsCount != 0 {
		t.Fatalf("RecordApiKeyUsage unexpectedly persisted synchronously: %+v", disk)
	}
	if err := FlushApiKeyUsage(); err != nil {
		t.Fatalf("FlushApiKeyUsage: %v", err)
	}
	if disk := persistedApiKey(t, path, created.ID); disk == nil || disk.TokensUsed != 10 || disk.CreditsUsed != 1.25 || disk.RequestsCount != 1 {
		t.Fatalf("flush did not persist usage: %+v", disk)
	}
}

func TestApiKeyUsageConcurrentRecordAndFlushIsExact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{Name: "concurrent", Key: "sk-concurrent", Enabled: true})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	const writers = 24
	const recordsPerWriter = 300
	start := make(chan struct{})
	errCh := make(chan error, writers+4)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for n := 0; n < recordsPerWriter; n++ {
				if err := RecordApiKeyUsage(created.ID, 3, 0.25); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for n := 0; n < 30; n++ {
				if err := FlushApiKeyUsage(); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent operation failed: %v", err)
	}
	if err := FlushApiKeyUsage(); err != nil {
		t.Fatalf("final flush: %v", err)
	}

	wantRequests := int64(writers * recordsPerWriter)
	wantTokens := wantRequests * 3
	wantCredits := float64(wantRequests) * 0.25
	for name, entry := range map[string]*ApiKeyEntry{
		"memory": GetApiKeyEntry(created.ID),
		"disk":   persistedApiKey(t, path, created.ID),
	} {
		if entry == nil || entry.RequestsCount != wantRequests || entry.TokensUsed != wantTokens || entry.CreditsUsed != wantCredits {
			t.Fatalf("%s totals = %+v, want requests=%d tokens=%d credits=%v", name, entry, wantRequests, wantTokens, wantCredits)
		}
	}
}

func TestApiKeyUsageFlushFailureRestoresPendingForRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{Name: "retry", Key: "sk-retry", Enabled: true})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	if err := RecordApiKeyUsage(created.ID, 11, 0.75); err != nil {
		t.Fatalf("RecordApiKeyUsage: %v", err)
	}

	restorePath := setConfigPathForTest(t, t.TempDir()) // atomic rename cannot replace a directory
	if err := FlushApiKeyUsage(); err == nil {
		restorePath()
		t.Fatal("expected flush persistence failure")
	}
	if got := GetApiKeyEntry(created.ID); got == nil || got.TokensUsed != 11 || got.CreditsUsed != 0.75 || got.RequestsCount != 1 {
		restorePath()
		t.Fatalf("failed flush lost pending usage: %+v", got)
	}
	if err := RecordApiKeyUsage(created.ID, 7, 0.25); err != nil {
		restorePath()
		t.Fatalf("record after failed flush: %v", err)
	}
	restorePath()

	if err := FlushApiKeyUsage(); err != nil {
		t.Fatalf("retry flush: %v", err)
	}
	if disk := persistedApiKey(t, path, created.ID); disk == nil || disk.TokensUsed != 18 || disk.CreditsUsed != 1 || disk.RequestsCount != 2 {
		t.Fatalf("retry did not persist all usage exactly once: %+v", disk)
	}
}

func TestApiKeyPendingUsageSurvivesUpdateAndFailedMutations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{Name: "mutations", Key: "sk-mutations", Enabled: true})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	if err := RecordApiKeyUsage(created.ID, 5, 0.5); err != nil {
		t.Fatalf("RecordApiKeyUsage: %v", err)
	}
	newLimit := int64(100)
	if err := PatchApiKey(created.ID, ApiKeyPatch{TokenLimit: &newLimit}); err != nil {
		t.Fatalf("PatchApiKey: %v", err)
	}
	// The update's config write may include pending usage, but it must remain a
	// pending in-memory delta until flush and therefore must not be double-counted.
	if disk := persistedApiKey(t, path, created.ID); disk == nil || disk.TokensUsed != 5 {
		t.Fatalf("config update did not snapshot pending usage: %+v", disk)
	}
	if err := FlushApiKeyUsage(); err != nil {
		t.Fatalf("FlushApiKeyUsage: %v", err)
	}
	if disk := persistedApiKey(t, path, created.ID); disk == nil || disk.TokensUsed != 5 || disk.RequestsCount != 1 {
		t.Fatalf("usage was double-counted after update+flush: %+v", disk)
	}

	if err := RecordApiKeyUsage(created.ID, 7, 0.25); err != nil {
		t.Fatalf("second record: %v", err)
	}
	restorePath := setConfigPathForTest(t, t.TempDir())
	if err := ResetApiKeyUsage(created.ID); err == nil {
		restorePath()
		t.Fatal("expected reset persistence failure")
	}
	if got := GetApiKeyEntry(created.ID); got == nil || got.TokensUsed != 12 || got.CreditsUsed != 0.75 || got.RequestsCount != 2 {
		restorePath()
		t.Fatalf("failed reset lost usage: %+v", got)
	}
	if err := DeleteApiKey(created.ID); err == nil {
		restorePath()
		t.Fatal("expected delete persistence failure")
	}
	if got := GetApiKeyEntry(created.ID); got == nil || got.TokensUsed != 12 || got.RequestsCount != 2 {
		restorePath()
		t.Fatalf("failed delete lost usage or key state: %+v", got)
	}
	restorePath()

	if err := ResetApiKeyUsage(created.ID); err != nil {
		t.Fatalf("successful reset: %v", err)
	}
	if got := GetApiKeyEntry(created.ID); got == nil || got.TokensUsed != 0 || got.CreditsUsed != 0 || got.RequestsCount != 0 || got.LastUsedAt == 0 {
		t.Fatalf("reset did not clear usage while preserving LastUsedAt: %+v", got)
	}
	if err := RecordApiKeyUsage(created.ID, 3, 0.125); err != nil {
		t.Fatalf("record before delete: %v", err)
	}
	if err := DeleteApiKey(created.ID); err != nil {
		t.Fatalf("DeleteApiKey: %v", err)
	}
	if err := RecordApiKeyUsage(created.ID, 1, 1); !errors.Is(err, ErrApiKeyNotFound) {
		t.Fatalf("record after delete error = %v, want ErrApiKeyNotFound", err)
	}
	if disk := persistedApiKey(t, path, created.ID); disk != nil {
		t.Fatalf("deleted key remained on disk: %+v", disk)
	}
}

func TestApiKeyUsageFlusherRetriesAndCloseFlushes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{Name: "background", Key: "sk-background", Enabled: true})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	if err := RecordApiKeyUsage(created.ID, 9, 0.5); err != nil {
		t.Fatalf("RecordApiKeyUsage: %v", err)
	}

	restorePath := setConfigPathForTest(t, t.TempDir())
	errorsSeen := make(chan error, 2)
	flusher := StartApiKeyUsageFlusher(time.Second, func(err error) {
		select {
		case errorsSeen <- err:
		default:
		}
	})
	select {
	case <-errorsSeen:
	case <-time.After(2500 * time.Millisecond):
		restorePath()
		_ = flusher.Close()
		t.Fatal("background flusher did not report the failed periodic write")
	}
	restorePath()

	deadline := time.Now().Add(2500 * time.Millisecond)
	for {
		disk := persistedApiKey(t, path, created.ID)
		if disk != nil && disk.TokensUsed == 9 && disk.RequestsCount == 1 {
			break
		}
		if time.Now().After(deadline) {
			_ = flusher.Close()
			t.Fatalf("background flusher did not retry restored pending usage: %+v", disk)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := RecordApiKeyUsage(created.ID, 4, 0.25); err != nil {
		t.Fatalf("record before Close: %v", err)
	}
	if err := flusher.Close(); err != nil {
		t.Fatalf("Close final flush: %v", err)
	}
	if disk := persistedApiKey(t, path, created.ID); disk == nil || disk.TokensUsed != 13 || disk.CreditsUsed != 0.75 || disk.RequestsCount != 2 {
		t.Fatalf("Close did not perform final flush: %+v", disk)
	}
}
