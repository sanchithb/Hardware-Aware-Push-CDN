// Package watcher recursively watches a content directory and emits a
// debounced event per file once writes quiesce. Encoders (ffmpeg et al.)
// write segments incrementally; the trailing-edge debounce ensures a file
// is pushed exactly once, complete, instead of once per write syscall.
package watcher

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher emits stable file paths on C.
type Watcher struct {
	C        <-chan string
	c        chan string
	root     string
	window   time.Duration
	log      *slog.Logger
	fsw      *fsnotify.Watcher
	mu       sync.Mutex
	pending  map[string]*time.Timer
	isIgnore func(string) bool
}

// New creates a Watcher for root with the given debounce window.
// ignore, when non-nil, filters paths (return true to skip).
func New(root string, window time.Duration, ignore func(string) bool, log *slog.Logger) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if window <= 0 {
		window = 200 * time.Millisecond
	}
	c := make(chan string, 1024)
	w := &Watcher{
		C: c, c: c, root: root, window: window, log: log, fsw: fsw,
		pending: map[string]*time.Timer{}, isIgnore: ignore,
	}
	if err := w.addRecursive(root); err != nil {
		fsw.Close()
		return nil, err
	}
	return w, nil
}

func (w *Watcher) addRecursive(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, don't abort the watcher
		}
		if d.IsDir() {
			if aerr := w.fsw.Add(p); aerr != nil {
				w.log.Warn("cannot watch directory", "dir", p, "err", aerr)
			} else {
				w.log.Debug("watching directory", "dir", p)
			}
		}
		return nil
	})
}

func isTempFile(p string) bool {
	base := filepath.Base(p)
	return strings.HasPrefix(base, ".") || strings.HasSuffix(base, ".tmp") ||
		strings.HasSuffix(base, ".swp") || strings.HasSuffix(base, "~")
}

// Run processes fsnotify events until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	defer w.fsw.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handle(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.log.Warn("watcher error", "err", err)
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event) {
	if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 {
		return
	}
	info, err := os.Stat(ev.Name)
	if err != nil {
		return
	}
	if info.IsDir() {
		if ev.Op&fsnotify.Create != 0 {
			_ = w.addRecursive(ev.Name)
		}
		return
	}
	if isTempFile(ev.Name) || (w.isIgnore != nil && w.isIgnore(ev.Name)) {
		return
	}

	// Trailing-edge debounce: every event replaces the pending timer for
	// the path. The fire callback checks it is still the *current* timer
	// before emitting — a timer that fired concurrently with a Reset would
	// otherwise emit a duplicate.
	name := ev.Name
	w.mu.Lock()
	defer w.mu.Unlock()
	if old, ok := w.pending[name]; ok {
		old.Stop()
	}
	var tm *time.Timer
	tm = time.AfterFunc(w.window, func() {
		w.mu.Lock()
		if w.pending[name] != tm {
			w.mu.Unlock()
			return // superseded by a newer write
		}
		delete(w.pending, name)
		w.mu.Unlock()
		select {
		case w.c <- name:
		default:
			w.log.Warn("watcher queue full, dropping event", "path", name)
		}
	})
	w.pending[name] = tm
}
