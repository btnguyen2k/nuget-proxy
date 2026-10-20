package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testCache(t *testing.T, directory string) *Cache {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	storage, err := New(ctx, Options{
		Dir: directory, MaxBytes: 150 << 10, MaxObjectBytes: 70 << 10, FetchTimeout: 5 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return storage
}

func response(body string) *http.Response {
	return &http.Response{
		StatusCode: 200, Header: http.Header{"Content-Type": {"application/octet-stream"}, "Etag": {`"upstream-etag"`}},
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)),
	}
}

func fetchBody(body string) Fetcher {
	return func(context.Context, string, string) (*http.Response, error) { return response(body), nil }
}

func readObject(t *testing.T, object *Object, want string) {
	t.Helper()
	data, err := io.ReadAll(object)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("cached body differs: got %d bytes, want %d", len(data), len(want))
	}
	if err := object.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPersistenceAndRevalidation(t *testing.T) {
	directory := t.TempDir()
	storage := testCache(t, directory)
	object, err := storage.Get(context.Background(), "key", time.Hour, fetchBody("unchanged bytes"))
	if err != nil {
		t.Fatal(err)
	}
	readObject(t, object, "unchanged bytes")
	storage = testCache(t, directory)
	noFetch := func(context.Context, string, string) (*http.Response, error) {
		t.Error("fresh persisted object was fetched again")
		return nil, errors.New("unexpected fetch")
	}
	object, err = storage.Get(context.Background(), "key", time.Hour, noFetch)
	if err != nil {
		t.Fatal(err)
	}
	readObject(t, object, "unchanged bytes")
	storage.mu.Lock()
	storage.current["key"].checkedAt = time.Now().Add(-2 * time.Hour)
	storage.mu.Unlock()
	object, err = storage.Get(context.Background(), "key", time.Hour, func(_ context.Context, etag, _ string) (*http.Response, error) {
		if etag != `"upstream-etag"` {
			t.Errorf("revalidation ETag = %q", etag)
		}
		return &http.Response{StatusCode: 304, Body: http.NoBody}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	readObject(t, object, "unchanged bytes")
	storage = testCache(t, directory)
	object, err = storage.Get(context.Background(), "key", time.Hour, noFetch)
	if err != nil {
		t.Fatal(err)
	}
	readObject(t, object, "unchanged bytes")
}

func TestCoalescingAndCanceledWaiter(t *testing.T) {
	storage := testCache(t, t.TempDir())
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fetch := func(ctx context.Context, _, _ string) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-finish:
			return response("shared"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		object, err := storage.Get(ctx, "key", time.Hour, fetch)
		if object != nil {
			object.Close()
		}
		first <- err
	}()
	<-started
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter: %v", err)
	}
	var workers sync.WaitGroup
	failures := make(chan error, 20)
	for range 20 {
		workers.Go(func() {
			object, err := storage.Get(context.Background(), "key", time.Hour, fetch)
			if err == nil {
				data, readErr := io.ReadAll(object)
				err = errors.Join(readErr, object.Close())
				if string(data) != "shared" {
					err = errors.Join(err, errors.New("incorrect coalesced body"))
				}
			}
			failures <- err
		})
	}
	close(finish)
	workers.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Error(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fetched %d times for one concurrent cache miss", calls.Load())
	}
}

func TestBoundedCacheAndActiveReaders(t *testing.T) {
	storage := testCache(t, t.TempDir())
	body := strings.Repeat("x", 64<<10)
	first, err := storage.Get(context.Background(), "first", 0, fetchBody(body))
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.Get(context.Background(), "second", 0, fetchBody(body))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Get(context.Background(), "third", 0, fetchBody(body)); !errors.Is(err, ErrFull) {
		t.Fatalf("expected full cache with pinned readers, got %v", err)
	}
	readObject(t, first, body)
	readObject(t, second, body)
	third, err := storage.Get(context.Background(), "third", 0, fetchBody(body))
	if err != nil {
		t.Fatal(err)
	}
	readObject(t, third, body)
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if storage.used+storage.reserved > storage.options.MaxBytes || storage.reserved != 0 {
		t.Fatalf("cache budget exceeded: used=%d, reserved=%d", storage.used, storage.reserved)
	}
	if storage.current["first"] != nil || storage.current["second"] == nil || storage.current["third"] == nil {
		t.Fatal("least-recently-used entry was not evicted")
	}
	var diskBytes int64
	for _, entry := range storage.files {
		info, err := os.Stat(entry.path)
		if err != nil {
			t.Fatal(err)
		}
		diskBytes += info.Size()
	}
	if diskBytes != storage.used {
		t.Fatalf("disk usage %d differs from accounting %d", diskBytes, storage.used)
	}
}

func TestIncompleteAndOversizedObjectsAreNotPublished(t *testing.T) {
	for _, name := range []string{"known oversized", "unknown oversized", "truncated", "interrupted", "not found"} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			storage := testCache(t, directory)
			fetch := func(context.Context, string, string) (*http.Response, error) {
				switch name {
				case "known oversized":
					return response(strings.Repeat("x", int(storage.options.MaxObjectBytes)+1)), nil
				case "unknown oversized":
					result := response(strings.Repeat("x", int(storage.options.MaxObjectBytes)+1))
					result.ContentLength = -1
					return result, nil
				case "truncated":
					result := response("short")
					result.ContentLength = 100
					return result, nil
				case "interrupted":
					result := response("")
					result.ContentLength = -1
					result.Body = io.NopCloser(io.MultiReader(strings.NewReader("partial"), failingReader{}))
					return result, nil
				default:
					return &http.Response{StatusCode: 404, Body: http.NoBody}, nil
				}
			}
			if _, err := storage.Get(context.Background(), "key", time.Hour, fetch); err == nil {
				t.Fatal("failed download succeeded")
			} else if strings.Contains(name, "oversized") && !errors.Is(err, ErrTooLarge) {
				t.Fatalf("expected size-limit failure, got %v", err)
			}
			files, err := os.ReadDir(directory)
			if err != nil || len(files) != 0 || storage.used != 0 || storage.reserved != 0 {
				t.Fatalf("failed download left files or reservations: %v, %v, used=%d, reserved=%d", files, err, storage.used, storage.reserved)
			}
			object, err := storage.Get(context.Background(), "key", time.Hour, fetchBody("recovered"))
			if err != nil {
				t.Fatal(err)
			}
			readObject(t, object, "recovered")
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection interrupted") }

func TestLoadCleansOnlyOwnedIncompleteOrInvalidFiles(t *testing.T) {
	directory := t.TempDir()
	invalid := digest("invalid") + "-123.cache"
	for _, name := range []string{invalid, "nuget-cache-orphan.tmp", "unrelated.txt"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("invalid"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	testCache(t, directory)
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 1 || files[0].Name() != "unrelated.txt" {
		t.Fatalf("unexpected files after recovery: %v, %v", files, err)
	}
}

func TestExpiredCacheDoesNotHideUpstreamFailure(t *testing.T) {
	storage := testCache(t, t.TempDir())
	object, err := storage.Get(context.Background(), "key", time.Hour, fetchBody("stale"))
	if err != nil {
		t.Fatal(err)
	}
	readObject(t, object, "stale")
	storage.current["key"].checkedAt = time.Now().Add(-2 * time.Hour)
	_, err = storage.Get(context.Background(), "key", time.Hour, func(context.Context, string, string) (*http.Response, error) {
		return nil, errors.New("upstream offline")
	})
	var fetchError *FetchError
	if !errors.As(err, &fetchError) {
		t.Fatalf("expected explicit upstream failure, got %v", err)
	}
}

func TestUnknownLengthPersistsExactBytes(t *testing.T) {
	directory := t.TempDir()
	storage := testCache(t, directory)
	body := string(bytes.Repeat([]byte{0, 1, 2, 255}, 1000))
	object, err := storage.Get(context.Background(), "key", 0, func(context.Context, string, string) (*http.Response, error) {
		result := response(body)
		result.ContentLength = -1
		return result, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	readObject(t, object, body)
	storage = testCache(t, directory)
	object, err = storage.Get(context.Background(), "key", 0, fetchBody("must not replace the cached bytes"))
	if err != nil {
		t.Fatal(err)
	}
	readObject(t, object, body)
}

func TestTruncatedPersistedObjectIsDiscarded(t *testing.T) {
	directory := t.TempDir()
	storage := testCache(t, directory)
	object, err := storage.Get(context.Background(), "key", 0, func(context.Context, string, string) (*http.Response, error) {
		result := response("complete body")
		result.ContentLength = -1
		return result, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	readObject(t, object, "complete body")
	e := storage.current["key"]
	if err := os.Truncate(e.path, e.size-1); err != nil {
		t.Fatal(err)
	}
	storage = testCache(t, directory)
	if len(storage.current) != 0 {
		t.Fatal("truncated persisted object was accepted")
	}
	object, err = storage.Get(context.Background(), "key", 0, fetchBody("fresh body"))
	if err != nil {
		t.Fatal(err)
	}
	readObject(t, object, "fresh body")
}

func TestReducedObjectLimitAppliesAfterRestart(t *testing.T) {
	directory := t.TempDir()
	storage := testCache(t, directory)
	object, err := storage.Get(context.Background(), "key", 0, fetchBody(strings.Repeat("x", 64<<10)))
	if err != nil {
		t.Fatal(err)
	}
	if err := object.Close(); err != nil {
		t.Fatal(err)
	}
	options := storage.options
	options.MaxObjectBytes = 32 << 10
	restarted, err := New(context.Background(), options, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.current) != 0 || restarted.used != 0 {
		t.Fatal("oversized persisted object survived the reduced object limit")
	}
}

func TestReplacementPreservesActiveReader(t *testing.T) {
	storage := testCache(t, t.TempDir())
	first, err := storage.Get(context.Background(), "key", time.Hour, fetchBody("original"))
	if err != nil {
		t.Fatal(err)
	}
	oldPath := storage.current["key"].path
	storage.current["key"].checkedAt = time.Now().Add(-2 * time.Hour)
	replacement, err := storage.Get(context.Background(), "key", time.Hour, fetchBody("replacement"))
	if err != nil {
		t.Fatal(err)
	}
	readObject(t, replacement, "replacement")
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("replacement removed a pinned object: %v", err)
	}
	readObject(t, first, "original")
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired file was not removed after closing its reader: %v", err)
	}
}
