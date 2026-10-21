package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const maxMetadataBytes = (64 << 10) - 4

var (
	ErrFull     = errors.New("cache capacity is occupied by active requests")
	ErrTooLarge = errors.New("upstream object exceeds cache.max_object_bytes")
)

type Options struct {
	Dir            string
	MaxBytes       int64
	MaxObjectBytes int64
	FetchTimeout   time.Duration
}

type Fetcher func(ctx context.Context, etag, lastModified string) (*http.Response, error)

type FetchError struct{ Err error }

func (e *FetchError) Error() string { return "fetch upstream object: " + e.Err.Error() }
func (e *FetchError) Unwrap() error { return e.Err }

type HTTPError struct {
	StatusCode int
	RetryAfter string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("upstream returned HTTP %d", e.StatusCode) }

type metadata struct {
	Key          string `json:"key"`
	ContentType  string `json:"content_type"`
	ETag         string `json:"etag"`
	LastModified string `json:"last_modified"`
	Length       int64  `json:"length"`
}

type entry struct {
	meta       metadata
	path       string
	offset     int64
	size       int64
	checkedAt  time.Time
	lastUsedAt time.Time
	refs       int
}

type Cache struct {
	ctx      context.Context
	options  Options
	logger   *slog.Logger
	mu       sync.Mutex
	current  map[string]*entry
	files    map[string]*entry
	used     int64
	reserved int64
	flights  singleflight.Group
}

type Object struct {
	*io.SectionReader
	ContentType  string
	ETag         string
	LastModified string
	Length       int64
	file         *os.File
	release      func() error
	once         sync.Once
	closeErr     error
}

func (o *Object) Close() error {
	o.once.Do(func() { o.closeErr = errors.Join(o.file.Close(), o.release()) })
	return o.closeErr
}

func New(ctx context.Context, options Options, logger *slog.Logger) (*Cache, error) {
	if options.MaxObjectBytes <= 0 || options.MaxBytes <= 64<<10 ||
		options.MaxObjectBytes > options.MaxBytes-(64<<10) || options.FetchTimeout <= 0 {
		return nil, errors.New("invalid cache limits or fetch timeout")
	}
	if err := os.MkdirAll(options.Dir, 0700); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	c := &Cache{
		ctx: ctx, options: options, logger: logger,
		current: make(map[string]*entry), files: make(map[string]*entry),
	}
	if err := c.load(); err != nil {
		return nil, err
	}
	if err := c.Check(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Cache) Check() error {
	file, err := os.CreateTemp(c.options.Dir, "nuget-cache-*.tmp")
	if err != nil {
		return fmt.Errorf("check cache directory: %w", err)
	}
	return errors.Join(file.Close(), os.Remove(file.Name()))
}

func (c *Cache) Get(ctx context.Context, key string, ttl time.Duration, fetch Fetcher) (*Object, error) {
	for attempt := 0; attempt < 3; attempt++ {
		object, err := c.openFresh(key, ttl)
		if object != nil || err != nil {
			return object, err
		}
		result := c.flights.DoChan(key, func() (any, error) {
			fetchCtx, cancel := context.WithTimeout(c.ctx, c.options.FetchTimeout)
			defer cancel()
			return nil, c.refresh(fetchCtx, key, ttl, fetch)
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case outcome := <-result:
			if outcome.Err != nil {
				return nil, outcome.Err
			}
		}
	}
	return nil, ErrFull
}

func (c *Cache) openFresh(key string, ttl time.Duration) (*Object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.current[key]
	if e == nil || (ttl > 0 && time.Since(e.checkedAt) >= ttl) {
		return nil, nil
	}
	file, err := os.Open(e.path)
	if err != nil {
		return nil, fmt.Errorf("open cached object: %w", err)
	}
	e.refs++
	e.lastUsedAt = time.Now()
	return &Object{
		SectionReader: io.NewSectionReader(file, e.offset, e.size-e.offset),
		ContentType:   e.meta.ContentType, ETag: e.meta.ETag, LastModified: e.meta.LastModified,
		Length: e.size - e.offset, file: file,
		release: func() error { return c.release(e) },
	}, nil
}

func (c *Cache) release(e *entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.refs--
	if e.refs == 0 && c.current[e.meta.Key] != e {
		return c.removeLocked(e)
	}
	return nil
}

func (c *Cache) refresh(ctx context.Context, key string, ttl time.Duration, fetch Fetcher) error {
	c.mu.Lock()
	old := c.current[key]
	if old != nil && (ttl == 0 || time.Since(old.checkedAt) < ttl) {
		c.mu.Unlock()
		return nil
	}
	var etag, modified string
	if old != nil {
		old.refs++
		etag, modified = old.meta.ETag, old.meta.LastModified
	}
	c.mu.Unlock()
	releaseOld := func() {
		if old != nil {
			if err := c.release(old); err != nil {
				c.logger.Error("remove replaced cache object", "error", err)
			}
			old = nil
		}
	}
	defer releaseOld()
	response, err := fetch(ctx, etag, modified)
	if err != nil {
		return &FetchError{Err: err}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		if old == nil {
			return &FetchError{Err: errors.New("upstream returned 304 without a cached object")}
		}
		now := time.Now()
		if err := os.Chtimes(old.path, now, now); err != nil {
			return fmt.Errorf("update cache validation time: %w", err)
		}
		c.mu.Lock()
		old.checkedAt = now
		c.mu.Unlock()
		return nil
	}
	if response.StatusCode != http.StatusOK {
		return &FetchError{Err: &HTTPError{StatusCode: response.StatusCode, RetryAfter: response.Header.Get("Retry-After")}}
	}
	releaseOld()
	return c.store(key, response)
}

func (c *Cache) store(key string, response *http.Response) (err error) {
	if response.ContentLength > c.options.MaxObjectBytes {
		return ErrTooLarge
	}
	meta := metadata{
		Key: key, ContentType: response.Header.Get("Content-Type"), ETag: response.Header.Get("ETag"),
		LastModified: response.Header.Get("Last-Modified"), Length: response.ContentLength,
	}
	header, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if meta.Length < 0 {
		// Reserve space to record the completed length without moving the body.
		header = append(header, bytes.Repeat([]byte{' '}, 20)...)
	}
	if len(header) > maxMetadataBytes {
		return &FetchError{Err: errors.New("upstream cache metadata is too large")}
	}
	limit := c.options.MaxObjectBytes
	if response.ContentLength >= 0 {
		limit = response.ContentLength
	}
	reservation := int64(4+len(header)) + limit
	if err := c.reserve(reservation); err != nil {
		return err
	}
	defer func() {
		c.mu.Lock()
		c.reserved -= reservation
		c.mu.Unlock()
	}()

	file, err := os.CreateTemp(c.options.Dir, "nuget-cache-*.tmp")
	if err != nil {
		return fmt.Errorf("create cache object: %w", err)
	}
	tempPath := file.Name()
	published := false
	defer func() {
		if !published {
			if closeErr := file.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
				err = errors.Join(err, closeErr)
			}
			if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
	}()
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(header)))
	if _, err := file.Write(prefix[:]); err != nil {
		return err
	}
	if _, err := file.Write(header); err != nil {
		return err
	}
	n, err := io.Copy(file, io.LimitReader(response.Body, limit))
	if err != nil {
		var diskError *os.PathError
		if errors.As(err, &diskError) {
			return err
		}
		return &FetchError{Err: err}
	}
	var extra [1]byte
	if count, readErr := io.ReadFull(response.Body, extra[:]); count > 0 {
		return ErrTooLarge
	} else if !errors.Is(readErr, io.EOF) {
		return &FetchError{Err: readErr}
	}
	if response.ContentLength >= 0 && n != response.ContentLength {
		return &FetchError{Err: io.ErrUnexpectedEOF}
	}
	if meta.Length < 0 {
		meta.Length = n
		completed, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		for i := range header {
			header[i] = ' '
		}
		copy(header, completed)
		if _, err := file.WriteAt(header, 4); err != nil {
			return err
		}
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	suffix := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(tempPath), "nuget-cache-"), ".tmp")
	finalPath := filepath.Join(c.options.Dir, digest(key)+"-"+suffix+".cache")
	if err := os.Rename(tempPath, finalPath); err != nil {
		return fmt.Errorf("publish cache object: %w", err)
	}
	published = true
	now := time.Now()
	e := &entry{meta: meta, path: finalPath, offset: int64(4 + len(header)), size: int64(4+len(header)) + n, checkedAt: now, lastUsedAt: now}
	c.mu.Lock()
	previous := c.current[key]
	c.current[key] = e
	c.files[finalPath] = e
	c.used += e.size
	// The committed file replaces its reservation while holding the same lock.
	c.reserved -= reservation
	reservation = 0
	if previous != nil && previous.refs == 0 {
		if err := c.removeLocked(previous); err != nil {
			c.logger.Error("remove replaced cache object", "error", err)
		}
	}
	c.mu.Unlock()
	return nil
}

func (c *Cache) reserve(size int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.used+c.reserved > c.options.MaxBytes-size {
		if err := c.evictLocked(); err != nil {
			return err
		}
	}
	c.reserved += size
	return nil
}

func (c *Cache) evictLocked() error {
	var oldest *entry
	for _, candidate := range c.files {
		if candidate.refs == 0 && (oldest == nil || candidate.lastUsedAt.Before(oldest.lastUsedAt)) {
			oldest = candidate
		}
	}
	if oldest == nil {
		return ErrFull
	}
	return c.removeLocked(oldest)
}

func (c *Cache) removeLocked(e *entry) error {
	if err := os.Remove(e.path); err != nil {
		return fmt.Errorf("remove cache object: %w", err)
	}
	if c.current[e.meta.Key] == e {
		delete(c.current, e.meta.Key)
	}
	delete(c.files, e.path)
	c.used -= e.size
	return nil
}

func (c *Cache) load() error {
	files, err := os.ReadDir(c.options.Dir)
	if err != nil {
		return fmt.Errorf("read cache directory: %w", err)
	}
	for _, file := range files {
		name := file.Name()
		if file.Type().IsRegular() && strings.HasPrefix(name, "nuget-cache-") && strings.HasSuffix(name, ".tmp") {
			if err := os.Remove(filepath.Join(c.options.Dir, name)); err != nil {
				return fmt.Errorf("remove incomplete cache object: %w", err)
			}
			c.logger.Warn("discard incomplete cache object", "file", name)
			continue
		}
		if !file.Type().IsRegular() || len(name) < 72 || !strings.HasSuffix(name, ".cache") || name[64] != '-' {
			continue
		}
		if _, err := hex.DecodeString(name[:64]); err != nil {
			continue
		}
		e, err := readEntry(filepath.Join(c.options.Dir, name))
		if err == nil && digest(e.meta.Key) != name[:64] {
			err = errors.New("cache key does not match filename")
		}
		if err == nil && e.size-e.offset > c.options.MaxObjectBytes {
			err = ErrTooLarge
		}
		if err != nil {
			c.logger.Warn("discard invalid cache object", "file", name, "error", err)
			if err := os.Remove(filepath.Join(c.options.Dir, name)); err != nil {
				return fmt.Errorf("remove invalid cache object: %w", err)
			}
			continue
		}
		c.files[e.path] = e
		c.used += e.size
		if previous := c.current[e.meta.Key]; previous != nil {
			if previous.checkedAt.After(e.checkedAt) {
				if err := c.removeLocked(e); err != nil {
					return err
				}
				continue
			}
			if err := c.removeLocked(previous); err != nil {
				return err
			}
		}
		c.current[e.meta.Key] = e
	}
	for c.used > c.options.MaxBytes {
		if err := c.evictLocked(); err != nil {
			return err
		}
	}
	return nil
}

func readEntry(filename string) (*entry, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	var prefix [4]byte
	if _, err := io.ReadFull(file, prefix[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(prefix[:])
	if length == 0 || length > maxMetadataBytes {
		return nil, errors.New("invalid cache header length")
	}
	header := make([]byte, length)
	if _, err := io.ReadFull(file, header); err != nil {
		return nil, err
	}
	var meta metadata
	if err := json.Unmarshal(header, &meta); err != nil {
		return nil, err
	}
	offset := int64(4 + length)
	if meta.Key == "" || meta.Length < 0 || info.Size() < offset || info.Size()-offset != meta.Length {
		return nil, errors.New("invalid cache object length or key")
	}
	return &entry{meta: meta, path: filename, offset: offset, size: info.Size(), checkedAt: info.ModTime(), lastUsedAt: info.ModTime()}, nil
}

func digest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
