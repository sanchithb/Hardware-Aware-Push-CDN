// Package logx configures structured logging for all hpcdn components and
// provides an in-memory ring buffer handler so recent log lines can be
// streamed to the web console without external log infrastructure.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// Entry is a captured log record kept in the ring buffer.
type Entry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Attrs   string    `json:"attrs,omitempty"`
}

// Ring is a fixed-size concurrent ring buffer of log entries.
type Ring struct {
	mu   sync.Mutex
	buf  []Entry
	next int
	full bool
}

// NewRing creates a ring buffer holding up to n entries.
func NewRing(n int) *Ring {
	if n <= 0 {
		n = 256
	}
	return &Ring{buf: make([]Entry, n)}
}

func (r *Ring) add(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// Snapshot returns entries oldest-first.
func (r *Ring) Snapshot() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		out := make([]Entry, r.next)
		copy(out, r.buf[:r.next])
		return out
	}
	out := make([]Entry, 0, len(r.buf))
	out = append(out, r.buf[r.next:]...)
	out = append(out, r.buf[:r.next]...)
	return out
}

// ringHandler tees records into a Ring while delegating to another handler.
type ringHandler struct {
	inner slog.Handler
	ring  *Ring
	attrs []slog.Attr
}

func (h *ringHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *ringHandler) Handle(ctx context.Context, rec slog.Record) error {
	var sb strings.Builder
	for _, a := range h.attrs {
		fmt.Fprintf(&sb, "%s=%v ", a.Key, a.Value)
	}
	rec.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&sb, "%s=%v ", a.Key, a.Value)
		return true
	})
	h.ring.add(Entry{
		Time:    rec.Time,
		Level:   rec.Level.String(),
		Message: rec.Message,
		Attrs:   strings.TrimSpace(sb.String()),
	})
	return h.inner.Handle(ctx, rec)
}

func (h *ringHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	na := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	na = append(na, h.attrs...)
	na = append(na, attrs...)
	return &ringHandler{inner: h.inner.WithAttrs(attrs), ring: h.ring, attrs: na}
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	return &ringHandler{inner: h.inner.WithGroup(name), ring: h.ring, attrs: h.attrs}
}

// Options controls logger construction.
type Options struct {
	Level  string // "debug", "info", "warn", "error"
	Format string // "text" or "json"
	Writer io.Writer
	Ring   *Ring // optional: tee records into this ring buffer
}

// New builds a slog.Logger from Options.
func New(opts Options) *slog.Logger {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	var lvl slog.Level
	switch strings.ToLower(opts.Level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	hopts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.ToLower(opts.Format) == "json" {
		h = slog.NewJSONHandler(w, hopts)
	} else {
		h = slog.NewTextHandler(w, hopts)
	}
	if opts.Ring != nil {
		h = &ringHandler{inner: h, ring: opts.Ring}
	}
	return slog.New(h)
}
