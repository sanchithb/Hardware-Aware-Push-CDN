// Package serve implements the edge node's data plane:
//
//   - PUT /ingest/{path}: authenticated content push from origins, with
//     SHA-256 integrity verification.
//   - GET /play/{path}: signed playback with HLS-correct caching headers
//     (short-TTL playlists, immutable segments), byte ranges, uniform
//     CORS, hold-and-serve for the live edge, and pull-through to the
//     origin on cache miss with request collapsing (singleflight) so a
//     thundering herd on a cold object costs one origin fetch.
//   - Playlist rewriting injects the validated signature into every
//     segment URI, optionally pointing segments back through the
//     controller so a player is re-routed mid-stream if this edge dies.
package serve

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/edge/cache"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/auth"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/metrics"
)

// RewriteMode controls how playlist segment URIs are rewritten.
type RewriteMode string

// Rewrite modes.
const (
	RewriteController RewriteMode = "controller" // absolute URLs through the controller (mid-stream failover)
	RewriteLocal      RewriteMode = "local"      // relative URIs with signature appended (stick to this edge)
	RewriteOff        RewriteMode = "off"
)

// Server is the edge data plane.
type Server struct {
	Cache         *cache.Cache
	Log           *slog.Logger
	Metrics       *metrics.Registry
	SigningKey    string
	IngestKey     string
	ControllerURL string // public controller URL for failover rewrites
	Rewrite       RewriteMode
	HoldTimeout   time.Duration
	PlaylistTTL   time.Duration
	SegmentTTL    time.Duration
	FetchClient   *http.Client

	origins  atomic.Value // []protocol.OriginEndpoint
	draining atomic.Bool

	activeConns atomic.Int64
	bytesOut    atomic.Uint64
	bytesIn     atomic.Uint64

	sf      singleflight.Group
	waitMu  sync.Mutex
	waiters map[string]*waiter

	playTotal  *metrics.Counter
	playDur    *metrics.Histogram
	pulls      *metrics.Counter
	ingests    *metrics.Counter
	authRejects *metrics.Counter
}

// SetOrigins is called from the heartbeat topology callback.
func (s *Server) SetOrigins(o []protocol.OriginEndpoint) { s.origins.Store(o) }

// SetDraining flips drain mode (from controller directives).
func (s *Server) SetDraining(v bool) { s.draining.Store(v) }

// Draining reports drain mode.
func (s *Server) Draining() bool { return s.draining.Load() }

// Snapshot implements nodeagent.Stats.
func (s *Server) Snapshot() (int, uint64, uint64, uint64, uint64, uint64, int) {
	hits, misses, _, bytes, files := s.Cache.Stats()
	return int(s.activeConns.Load()), s.bytesOut.Load(), s.bytesIn.Load(),
		hits, misses, uint64(bytes), files
}

// Mount registers edge routes.
func (s *Server) Mount(mux *http.ServeMux) {
	if s.waiters == nil {
		s.waiters = map[string]*waiter{}
	}
	if s.FetchClient == nil {
		s.FetchClient = &http.Client{Timeout: 30 * time.Second}
	}
	s.playTotal = s.Metrics.Counter("hpcdn_edge_play_requests_total", "Playback requests served", nil)
	s.playDur = s.Metrics.Histogram("hpcdn_edge_play_duration_seconds", "Playback request latency", nil)
	s.pulls = s.Metrics.Counter("hpcdn_edge_origin_pulls_total", "Cache misses satisfied by origin pull-through", nil)
	s.ingests = s.Metrics.Counter("hpcdn_edge_ingest_total", "Objects ingested via origin push", nil)
	s.authRejects = s.Metrics.Counter("hpcdn_edge_auth_rejects_total", "Rejected playback/ingest auth attempts", nil)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /metrics", s.Metrics.Handler())
	mux.HandleFunc("PUT /ingest/", s.handleIngest)
	mux.HandleFunc("/play/", s.handlePlay)
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	got := r.Header.Get(protocol.HeaderIngestKey)
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.IngestKey)) != 1 {
		s.authRejects.Inc()
		http.Error(w, "invalid ingest key", http.StatusUnauthorized)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/ingest/")
	if key == "" {
		http.Error(w, "missing object path", http.StatusBadRequest)
		return
	}
	n, err := s.Cache.Put(key, r.Body, r.Header.Get(protocol.HeaderChecksum))
	if err != nil {
		code := http.StatusInternalServerError
		if err == cache.ErrChecksum {
			code = http.StatusUnprocessableEntity
		}
		s.Log.Warn("ingest failed", "key", key, "err", err)
		http.Error(w, err.Error(), code)
		return
	}
	s.bytesIn.Add(uint64(n))
	s.ingests.Inc()
	s.notifyArrival(cache.Normalize(key))
	s.Log.Debug("ingested object", "key", key, "bytes", n)
	w.WriteHeader(http.StatusCreated)
}

// waiter is a shared arrival signal with a reference count so abandoned
// waits (objects that never arrive) don't leak map entries.
type waiter struct {
	ch   chan struct{}
	refs int
}

// notifyArrival wakes hold-and-serve waiters for key.
func (s *Server) notifyArrival(key string) {
	s.waitMu.Lock()
	if wt, ok := s.waiters[key]; ok {
		close(wt.ch)
		delete(s.waiters, key)
	}
	s.waitMu.Unlock()
}

// arrivalChan returns a channel closed when key is ingested, plus a
// release func the caller must invoke when done waiting.
func (s *Server) arrivalChan(key string) (<-chan struct{}, func()) {
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	wt, ok := s.waiters[key]
	if !ok {
		wt = &waiter{ch: make(chan struct{})}
		s.waiters[key] = wt
	}
	wt.refs++
	return wt.ch, func() {
		s.waitMu.Lock()
		defer s.waitMu.Unlock()
		wt.refs--
		if wt.refs <= 0 && s.waiters[key] == wt {
			delete(s.waiters, key)
		}
	}
}

func corsHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Range, Origin, Accept")
	h.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")
}

func isPlaylist(key string) bool {
	return strings.HasSuffix(key, ".m3u8") || strings.HasSuffix(key, ".mpd")
}

// countingWriter tallies response bytes for telemetry.
type countingWriter struct {
	http.ResponseWriter
	n *atomic.Uint64
}

func (cw *countingWriter) Write(b []byte) (int, error) {
	n, err := cw.ResponseWriter.Write(b)
	cw.n.Add(uint64(n))
	return n, err
}

func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		corsHeaders(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()
	s.activeConns.Add(1)
	defer s.activeConns.Add(-1)

	corsHeaders(w)

	if s.draining.Load() {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "edge is draining", http.StatusServiceUnavailable)
		return
	}
	if err := auth.ValidateQuery(s.SigningKey, r.URL.Path, r.URL.Query(), time.Now()); err != nil {
		s.authRejects.Inc()
		s.Log.Warn("playback rejected", "path", r.URL.Path, "reason", err)
		http.Error(w, "invalid or expired stream signature", http.StatusForbidden)
		return
	}

	key := cache.Normalize(strings.TrimPrefix(r.URL.Path, "/play/"))
	if key == "" {
		http.Error(w, "missing object path", http.StatusBadRequest)
		return
	}

	f, info, ok := s.Cache.Open(key)
	if !ok {
		s.Cache.Miss()
		var err error
		f, info, err = s.acquireMissing(r.Context(), key)
		if err != nil {
			s.Log.Warn("object unavailable", "key", key, "err", err)
			http.Error(w, "content not available", http.StatusNotFound)
			return
		}
	}
	defer f.Close()

	if isPlaylist(key) {
		s.servePlaylist(w, r, f, info.ModTime())
	} else {
		s.serveSegment(w, r, key, f, info)
	}
	s.playTotal.Inc()
	s.playDur.Observe(time.Since(start).Seconds())
}

// acquireMissing implements hold-and-serve + pull-through with collapsing.
// For live streams the object usually arrives via push within the hold
// window; otherwise one collapsed origin fetch fills the cache.
func (s *Server) acquireMissing(ctx context.Context, key string) (*os.File, os.FileInfo, error) {
	arrival, release := s.arrivalChan(key)
	defer release()

	hold := s.HoldTimeout
	if hold <= 0 {
		hold = 3 * time.Second
	}
	holdTimer := time.NewTimer(hold)
	defer holdTimer.Stop()

	// Start a collapsed origin pull immediately in parallel with the hold;
	// whichever fills the cache first wins.
	pullDone := make(chan error, 1)
	go func() {
		_, err, _ := s.sf.Do(key, func() (any, error) {
			if s.Cache.Contains(key) {
				return nil, nil
			}
			return nil, s.pullFromOrigin(ctx, key)
		})
		pullDone <- err
	}()

	var pullErr error
	for {
		if f, info, ok := s.Cache.Open(key); ok {
			return f, info, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-arrival:
			continue // pushed by origin; loop re-opens from cache
		case pullErr = <-pullDone:
			if pullErr == nil {
				continue // pulled; loop re-opens from cache
			}
			pullDone = nil // don't select on it again
			select {
			case <-arrival:
				continue
			case <-holdTimer.C:
				return nil, nil, fmt.Errorf("hold window elapsed after failed pull: %w", pullErr)
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		case <-holdTimer.C:
			if pullErr == nil {
				pullErr = fmt.Errorf("no origin delivered %q in time", key)
			}
			return nil, nil, pullErr
		}
	}
}

// pullFromOrigin fetches key from the first origin that has it.
func (s *Server) pullFromOrigin(ctx context.Context, key string) error {
	origins, _ := s.origins.Load().([]protocol.OriginEndpoint)
	if len(origins) == 0 {
		return fmt.Errorf("no origins known")
	}
	var lastErr error
	for _, o := range origins {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.FetchURL+"/"+key, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set(protocol.HeaderIngestKey, s.IngestKey)
		resp, err := s.FetchClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("origin %s returned %d", o.NodeID, resp.StatusCode)
			continue
		}
		n, err := s.Cache.Put(key, resp.Body, resp.Header.Get(protocol.HeaderChecksum))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		s.bytesIn.Add(uint64(n))
		s.pulls.Inc()
		s.notifyArrival(key)
		s.Log.Info("pull-through fill", "key", key, "origin", o.NodeID, "bytes", n)
		return nil
	}
	return lastErr
}

// serveSegment serves media objects: immutable long-TTL caching, ranges
// and conditional requests via http.ServeContent.
func (s *Server) serveSegment(w http.ResponseWriter, r *http.Request, key string, f *os.File, info os.FileInfo) {
	ttl := int(s.SegmentTTL.Seconds())
	if ttl <= 0 {
		ttl = 31536000
	}
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d, immutable", ttl))
	w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, info.ModTime().UnixNano(), info.Size()))
	if ct := contentType(key); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	cw := &countingWriter{ResponseWriter: w, n: &s.bytesOut}
	http.ServeContent(cw, r, path.Base(key), info.ModTime(), f)
}

// servePlaylist rewrites segment URIs to carry the validated signature and
// serves with a short TTL so players re-poll promptly on live streams.
func (s *Server) servePlaylist(w http.ResponseWriter, r *http.Request, f *os.File, mod time.Time) {
	raw, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	body := s.rewritePlaylist(string(raw), r)

	ttl := int(s.PlaylistTTL.Seconds())
	if ttl < 1 {
		ttl = 1
	}
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", ttl))
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	cw := &countingWriter{ResponseWriter: w, n: &s.bytesOut}
	http.ServeContent(cw, r, "playlist.m3u8", mod, strings.NewReader(body))
}

// rewritePlaylist maps every media URI in an M3U8 document according to
// the rewrite mode, propagating the request's signature parameters.
func (s *Server) rewritePlaylist(m3u8 string, r *http.Request) string {
	if s.Rewrite == RewriteOff {
		return m3u8
	}
	sig := auth.SignatureParams(r.URL.Query()).Encode()
	base := path.Dir(strings.TrimPrefix(r.URL.Path, "/play/")) // stream dir, e.g. "stream1"

	mapURI := func(uri string) string {
		u := strings.TrimSpace(uri)
		if u == "" || strings.Contains(u, "://") {
			return uri // absolute remote URIs left untouched
		}
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		if s.Rewrite == RewriteController && s.ControllerURL != "" {
			abs := path.Join("/play", base, u)
			return s.ControllerURL + abs + sep + sig
		}
		return u + sep + sig
	}

	lines := strings.Split(m3u8, "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
		case strings.HasPrefix(t, "#"):
			// Rewrite URI="..." attributes (EXT-X-MAP, EXT-X-KEY, EXT-X-MEDIA…).
			if idx := strings.Index(t, `URI="`); idx >= 0 {
				startQ := idx + len(`URI="`)
				if endQ := strings.Index(t[startQ:], `"`); endQ >= 0 {
					uri := t[startQ : startQ+endQ]
					lines[i] = strings.Replace(line, `URI="`+uri+`"`, `URI="`+mapURI(uri)+`"`, 1)
				}
			}
		default:
			lines[i] = mapURI(line)
		}
	}
	return strings.Join(lines, "\n")
}

func contentType(key string) string {
	switch strings.ToLower(path.Ext(key)) {
	case ".ts":
		return "video/mp2t"
	case ".m4s", ".mp4":
		return "video/mp4"
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	case ".mpd":
		return "application/dash+xml"
	case ".vtt":
		return "text/vtt"
	case ".aac":
		return "audio/aac"
	default:
		return ""
	}
}
