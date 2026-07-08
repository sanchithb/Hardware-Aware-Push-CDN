package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func put(t *testing.T, c *Cache, key, content string) {
	t.Helper()
	if _, err := c.Put(key, strings.NewReader(content), ""); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func TestPutOpenRoundtrip(t *testing.T) {
	c, err := Open(t.TempDir(), 1<<20, testLog())
	if err != nil {
		t.Fatal(err)
	}
	put(t, c, "stream1/seg0.ts", "hello segment")
	f, info, ok := c.Open("stream1/seg0.ts")
	if !ok {
		t.Fatal("object missing after put")
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	if string(b) != "hello segment" {
		t.Fatalf("content mismatch: %q", b)
	}
	if info.Size() != int64(len("hello segment")) {
		t.Fatalf("size mismatch: %d", info.Size())
	}
	hits, misses, _, bytes, files := c.Stats()
	if hits != 1 || misses != 0 || files != 1 || bytes != info.Size() {
		t.Fatalf("stats wrong: hits=%d misses=%d files=%d bytes=%d", hits, misses, files, bytes)
	}
}

func TestChecksumVerification(t *testing.T) {
	c, _ := Open(t.TempDir(), 1<<20, testLog())
	content := "verified content"
	sum := sha256.Sum256([]byte(content))
	good := hex.EncodeToString(sum[:])

	if _, err := c.Put("a.ts", strings.NewReader(content), good); err != nil {
		t.Fatalf("correct checksum rejected: %v", err)
	}
	if _, err := c.Put("b.ts", strings.NewReader(content), strings.Repeat("0", 64)); err != ErrChecksum {
		t.Fatalf("bad checksum accepted: %v", err)
	}
	if c.Contains("b.ts") {
		t.Fatal("corrupt object became visible")
	}
}

func TestSieveEvictsUnvisitedFirst(t *testing.T) {
	// Budget of ~5 objects of 100 bytes.
	c, _ := Open(t.TempDir(), 500, testLog())
	blob := strings.Repeat("x", 100)
	for i := 0; i < 5; i++ {
		put(t, c, fmt.Sprintf("obj%d", i), blob)
	}
	// Touch obj0 and obj1 (visited bit set) — SIEVE must protect them.
	for _, k := range []string{"obj0", "obj1"} {
		f, _, ok := c.Open(k)
		if !ok {
			t.Fatalf("%s missing", k)
		}
		f.Close()
	}
	// Two inserts force two evictions.
	put(t, c, "obj5", blob)
	put(t, c, "obj6", blob)

	for _, k := range []string{"obj0", "obj1"} {
		if !c.Contains(k) {
			t.Errorf("visited object %s was evicted before unvisited ones", k)
		}
	}
	if c.Contains("obj2") && c.Contains("obj3") {
		t.Error("expected at least one unvisited object evicted")
	}
	_, _, ev, bytes, _ := c.Stats()
	if ev < 2 {
		t.Errorf("expected >=2 evictions, got %d", ev)
	}
	if bytes > 500 {
		t.Errorf("cache exceeds budget: %d", bytes)
	}
}

func TestPurgePrefix(t *testing.T) {
	c, _ := Open(t.TempDir(), 1<<20, testLog())
	put(t, c, "stream1/a.ts", "a")
	put(t, c, "stream1/b.ts", "b")
	put(t, c, "stream10/c.ts", "c") // prefix-sibling must survive
	put(t, c, "other/d.ts", "d")

	if n := c.Purge("stream1"); n != 2 {
		t.Fatalf("expected 2 purged, got %d", n)
	}
	if c.Contains("stream1/a.ts") || c.Contains("stream1/b.ts") {
		t.Error("purged objects still present")
	}
	if !c.Contains("stream10/c.ts") || !c.Contains("other/d.ts") {
		t.Error("purge removed objects outside the prefix")
	}
}

func TestPathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	c, _ := Open(dir, 1<<20, testLog())
	// Normalize collapses traversal; ensure nothing lands outside the root.
	if _, err := c.Put("../../evil.txt", strings.NewReader("x"), ""); err != nil {
		return // rejected outright is fine too
	}
	if f, _, ok := c.Open("../../evil.txt"); ok {
		f.Close()
		// The normalized key must resolve inside the cache dir.
		p, err := c.safePath(Normalize("../../evil.txt"))
		if err != nil || !strings.HasPrefix(p, dir) {
			t.Fatal("traversal escaped the cache root")
		}
	}
}

func TestIndexRebuildOnReopen(t *testing.T) {
	dir := t.TempDir()
	c, _ := Open(dir, 1<<20, testLog())
	put(t, c, "s/x.ts", "persisted")

	c2, err := Open(dir, 1<<20, testLog())
	if err != nil {
		t.Fatal(err)
	}
	f, _, ok := c2.Open("s/x.ts")
	if !ok {
		t.Fatal("object lost across restart")
	}
	f.Close()
	_, _, _, _, files := c2.Stats()
	if files != 1 {
		t.Fatalf("rebuilt index has %d files", files)
	}
}
