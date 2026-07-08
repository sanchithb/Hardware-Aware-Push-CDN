package watcher

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func collect(c <-chan string, window time.Duration) []string {
	var out []string
	deadline := time.After(window)
	for {
		select {
		case p := <-c:
			out = append(out, p)
		case <-deadline:
			return out
		}
	}
}

func TestDebounceCoalescesWrites(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, 250*time.Millisecond, nil, testLog())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Simulate an encoder writing a segment in many small appends. The OS
	// may batch write notifications, but every burst that lands inside the
	// window must collapse to a single push.
	p := filepath.Join(dir, "seg0.ts")
	f, _ := os.Create(p)
	for i := 0; i < 20; i++ {
		_, _ = f.WriteString("chunk")
	}
	f.Close()

	events := collect(w.C, 900*time.Millisecond)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 debounced event, got %d: %v", len(events), events)
	}
}

func TestNewSubdirectoryIsWatched(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, 100*time.Millisecond, nil, testLog())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	sub := filepath.Join(dir, "stream2")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond) // let the watcher pick up the dir
	if err := os.WriteFile(filepath.Join(sub, "a.ts"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	events := collect(w.C, 900*time.Millisecond)
	if len(events) != 1 {
		t.Fatalf("file in new subdirectory not detected: %v", events)
	}
}

func TestIgnoreFilter(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, 80*time.Millisecond, func(p string) bool {
		return filepath.Ext(p) == ".log"
	}, testLog())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	_ = os.WriteFile(filepath.Join(dir, "keep.ts"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "skip.log"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".hidden.ts"), []byte("x"), 0o644)

	events := collect(w.C, 600*time.Millisecond)
	if len(events) != 1 || filepath.Base(events[0]) != "keep.ts" {
		t.Fatalf("filter failed, events: %v", events)
	}
}
