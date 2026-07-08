// Package cache is the edge node's disk-backed content cache with SIEVE
// eviction (Zhang et al., NSDI 2024). SIEVE keeps a FIFO order with a
// "visited" bit and a hand that sweeps from oldest to newest evicting the
// first unvisited entry. It is simpler than LRU (no reordering on hit,
// so no lock contention on the hot read path beyond a bit flip) and
// consistently shows lower miss ratios than LRU/ARC on skewed CDN traces,
// which is why it has displaced plain LRU in modern caches.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrChecksum is returned when ingested content fails checksum verification.
var ErrChecksum = errors.New("cache: checksum mismatch")

// entry is one cached object in the SIEVE queue.
// prev points toward the tail (older), next toward the head (newer).
type entry struct {
	key        string
	size       int64
	visited    bool
	prev, next *entry
}

// Cache is a byte-bounded disk cache. All exported methods are safe for
// concurrent use.
type Cache struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	items    map[string]*entry
	head     *entry // newest
	tail     *entry // oldest
	hand     *entry // SIEVE eviction hand
	curBytes int64
	log      *slog.Logger

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

// Open initializes a cache rooted at dir, rebuilding the index from disk
// (oldest files enter the queue first, approximating pre-restart order).
func Open(dir string, maxBytes int64, log *slog.Logger) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cache: creating dir: %w", err)
	}
	c := &Cache{dir: dir, maxBytes: maxBytes, items: map[string]*entry{}, log: log}

	type found struct {
		key  string
		size int64
		mod  int64
	}
	var files []found
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		files = append(files, found{key: filepath.ToSlash(rel), size: info.Size(), mod: info.ModTime().UnixNano()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cache: scanning dir: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod < files[j].mod })
	for _, f := range files {
		c.insertLocked(f.key, f.size)
	}
	c.evictLocked()
	if len(files) > 0 {
		log.Info("cache index rebuilt", "files", len(files), "bytes", c.curBytes)
	}
	return c, nil
}

// safePath maps a cache key to an on-disk path, rejecting traversal.
func (c *Cache) safePath(key string) (string, error) {
	clean := path.Clean("/" + key)
	if strings.Contains(clean, "..") {
		return "", fmt.Errorf("cache: invalid key %q", key)
	}
	return filepath.Join(c.dir, filepath.FromSlash(strings.TrimPrefix(clean, "/"))), nil
}

// Normalize returns the canonical key form used by the index.
func Normalize(key string) string {
	return strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(key, "\\", "/")), "/")
}

// insertLocked adds a key at the head. Caller holds c.mu.
func (c *Cache) insertLocked(key string, size int64) {
	if old, ok := c.items[key]; ok {
		c.curBytes += size - old.size
		old.size = size
		old.visited = true
		return
	}
	e := &entry{key: key, size: size}
	e.prev = c.head
	if c.head != nil {
		c.head.next = e
	}
	c.head = e
	if c.tail == nil {
		c.tail = e
	}
	c.items[key] = e
	c.curBytes += size
}

// removeLocked unlinks an entry. Caller holds c.mu.
func (c *Cache) removeLocked(e *entry) {
	if c.hand == e {
		c.hand = e.next
	}
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		c.tail = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		c.head = e.prev
	}
	delete(c.items, e.key)
	c.curBytes -= e.size
}

// evictLocked runs the SIEVE hand until usage is within budget.
// Caller holds c.mu.
func (c *Cache) evictLocked() {
	if c.maxBytes <= 0 {
		return
	}
	for c.curBytes > c.maxBytes && c.tail != nil {
		if c.hand == nil {
			c.hand = c.tail
		}
		e := c.hand
		if e.visited {
			e.visited = false
			c.hand = e.next // sweep toward the head
			continue
		}
		c.hand = e.next
		c.removeLocked(e)
		c.evictions.Add(1)
		if p, err := c.safePath(e.key); err == nil {
			if rmErr := os.Remove(p); rmErr != nil && !os.IsNotExist(rmErr) {
				c.log.Warn("evicting file failed", "key", e.key, "err", rmErr)
			}
		}
	}
}

// Put streams content into the cache under key, verifying wantSHA256
// (hex, optional "") before the object becomes visible. Returns bytes written.
func (c *Cache) Put(key string, r io.Reader, wantSHA256 string) (int64, error) {
	key = Normalize(key)
	dst, err := c.safePath(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("cache: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".ingest-*")
	if err != nil {
		return 0, fmt.Errorf("cache: temp file: %w", err)
	}
	tmpName := tmp.Name()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	cerr := tmp.Close()
	if err != nil || cerr != nil {
		os.Remove(tmpName)
		if err == nil {
			err = cerr
		}
		return 0, fmt.Errorf("cache: writing object: %w", err)
	}
	if wantSHA256 != "" && !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), wantSHA256) {
		os.Remove(tmpName)
		return 0, ErrChecksum
	}
	if err := os.Rename(tmpName, dst); err != nil {
		// Windows cannot rename over an open file; retry after removing.
		os.Remove(dst)
		if err = os.Rename(tmpName, dst); err != nil {
			os.Remove(tmpName)
			return 0, fmt.Errorf("cache: publishing object: %w", err)
		}
	}
	c.mu.Lock()
	c.insertLocked(key, n)
	c.evictLocked()
	c.mu.Unlock()
	return n, nil
}

// Open returns a readable handle if key is cached, counting a hit and
// setting the SIEVE visited bit. A miss is only counted via Miss() so the
// caller can distinguish lookups it will satisfy by other means.
func (c *Cache) Open(key string) (*os.File, os.FileInfo, bool) {
	key = Normalize(key)
	c.mu.Lock()
	e, ok := c.items[key]
	if ok {
		e.visited = true
	}
	c.mu.Unlock()
	if !ok {
		return nil, nil, false
	}
	p, err := c.safePath(key)
	if err != nil {
		return nil, nil, false
	}
	f, err := os.Open(p)
	if err != nil {
		// Index/disk drift (manual deletion): heal the index.
		c.mu.Lock()
		if e2, ok2 := c.items[key]; ok2 {
			c.removeLocked(e2)
		}
		c.mu.Unlock()
		return nil, nil, false
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, false
	}
	c.hits.Add(1)
	return f, info, true
}

// Contains reports whether key is cached without touching hit counters.
func (c *Cache) Contains(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.items[Normalize(key)]
	return ok
}

// Miss records a cache miss.
func (c *Cache) Miss() { c.misses.Add(1) }

// Purge removes all objects whose key starts with prefix ("" purges all).
// Returns the number of objects removed.
func (c *Cache) Purge(prefix string) int {
	prefix = Normalize(prefix)
	c.mu.Lock()
	var victims []*entry
	for k, e := range c.items {
		if prefix == "" || k == prefix || strings.HasPrefix(k, prefix+"/") {
			victims = append(victims, e)
		}
	}
	for _, e := range victims {
		c.removeLocked(e)
	}
	c.mu.Unlock()
	for _, e := range victims {
		if p, err := c.safePath(e.key); err == nil {
			os.Remove(p)
		}
	}
	return len(victims)
}

// Stats returns lifetime counters and current usage.
func (c *Cache) Stats() (hits, misses, evictions uint64, bytes int64, files int) {
	c.mu.Lock()
	bytes = c.curBytes
	files = len(c.items)
	c.mu.Unlock()
	return c.hits.Load(), c.misses.Load(), c.evictions.Load(), bytes, files
}

// Dir returns the cache root directory.
func (c *Cache) Dir() string { return c.dir }
