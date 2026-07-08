// Package origin wires a runnable origin node: content watcher, push
// pipeline, the authenticated /fetch endpoint that backs edge
// pull-through, controller enrollment and heartbeats.
package origin

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/nodeagent"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/origin/pusher"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/origin/watcher"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/config"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/metrics"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/tlsutil"
)

// Origin is a runnable origin node.
type Origin struct {
	Cfg config.Origin
	Log *slog.Logger
}

// stats adapts origin counters to the nodeagent.Stats interface.
type stats struct {
	p        *pusher.Pusher
	bytesIn  *atomic.Uint64 // reserved: origin ingest API (future)
	files    *atomic.Int64
	fileSize *atomic.Int64
}

func (s *stats) Snapshot() (int, uint64, uint64, uint64, uint64, uint64, int) {
	return 0, s.p.BytesOut(), s.bytesIn.Load(), 0, 0, uint64(s.fileSize.Load()), int(s.files.Load())
}

// Run starts the origin node and blocks until ctx is cancelled.
func (o *Origin) Run(ctx context.Context) error {
	cfg := o.Cfg
	if cfg.Name == "" {
		if hn, err := os.Hostname(); err == nil {
			cfg.Name = hn
		}
	}
	if cfg.PublicURL == "" {
		cfg.PublicURL = deriveLocalURL(cfg.Listen, cfg.TLS.Enabled)
		o.Log.Warn("public_url not set; derived for single-machine use", "public_url", cfg.PublicURL)
	}
	watchDir, err := filepath.Abs(cfg.WatchDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(watchDir, 0o755); err != nil {
		return fmt.Errorf("origin: creating watch dir: %w", err)
	}

	mreg := metrics.NewRegistry()
	push := &pusher.Pusher{
		Root:    watchDir,
		Workers: cfg.PushWorkers,
		Retries: cfg.PushRetries,
		Log:     o.Log,
		Metrics: mreg,
		HTTP:    nodeagent.NewHTTPClient(cfg.InsecureSkipVerify, 30*time.Second),
	}

	st := &stats{p: push, bytesIn: &atomic.Uint64{}, files: &atomic.Int64{}, fileSize: &atomic.Int64{}}

	agent := &nodeagent.Agent{
		ControllerURL: strings.TrimRight(cfg.ControllerURL, "/"),
		Kind:          protocol.KindOrigin,
		Name:          cfg.Name,
		PublicURL:     strings.TrimRight(cfg.PublicURL, "/"),
		Region:        cfg.Region,
		DataDir:       cfg.DataDir,
		CacheDir:      watchDir,
		Log:           o.Log,
		Stats:         st,
		HTTP:          nodeagent.NewHTTPClient(cfg.InsecureSkipVerify, 10*time.Second),
		OnTopology: func(edges []protocol.EdgeEndpoint, _ []protocol.OriginEndpoint) {
			push.SetEdges(edges)
		},
	}
	if err := agent.Enroll(ctx, cfg.JoinToken); err != nil {
		return err
	}
	push.IngestKey = agent.Identity().IngestKey
	push.Start(ctx)

	// Eligible-extension filter for eager push.
	extOK := func(p string) bool {
		if len(cfg.PushExtensions) == 0 {
			return true
		}
		ext := strings.ToLower(filepath.Ext(p))
		for _, e := range cfg.PushExtensions {
			if ext == strings.ToLower(e) {
				return true
			}
		}
		return false
	}

	w, err := watcher.New(watchDir, cfg.DebounceWindow, func(p string) bool { return !extOK(p) }, o.Log)
	if err != nil {
		return fmt.Errorf("origin: starting watcher: %w", err)
	}
	go w.Run(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case abs := <-w.C:
				rel, rerr := pusher.RelKey(watchDir, abs)
				if rerr != nil {
					continue
				}
				push.Push(pusher.Job{AbsPath: abs, RelPath: rel})
			}
		}
	}()

	// Periodically account the content library for telemetry.
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			var count int64
			var size int64
			_ = filepath.WalkDir(watchDir, func(_ string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				if info, ierr := d.Info(); ierr == nil {
					count++
					size += info.Size()
				}
				return nil
			})
			st.files.Store(count)
			st.fileSize.Store(size)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	// HTTP: /fetch backs edge pull-through; auth by ingest key.
	ingestKey := agent.Identity().IngestKey
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /metrics", mreg.Handler())
	fetchTotal := mreg.Counter("hpcdn_origin_fetch_total", "Pull-through fetches served to edges", nil)
	mux.HandleFunc("GET /fetch/", func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(protocol.HeaderIngestKey)
		if subtle.ConstantTimeCompare([]byte(got), []byte(ingestKey)) != 1 {
			http.Error(w, "invalid ingest key", http.StatusUnauthorized)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/fetch/")
		clean := filepath.Clean(filepath.FromSlash(key))
		if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		full := filepath.Join(watchDir, clean)
		info, serr := os.Stat(full)
		if serr != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		fetchTotal.Inc()
		http.ServeFile(w, r, full)
	})

	httpSrv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("origin: listen %s: %w", cfg.Listen, err)
	}
	if cfg.TLS.Enabled && cfg.TLS.CertFile == "" {
		tc, terr := tlsutil.SelfSigned()
		if terr != nil {
			return terr
		}
		httpSrv.TLSConfig = tc
	}

	go agent.RunHeartbeats(ctx)

	o.Log.Info("origin node running",
		"addr", ln.Addr().String(), "node_id", agent.Identity().NodeID,
		"watch_dir", watchDir, "workers", cfg.PushWorkers)

	errCh := make(chan error, 1)
	go func() {
		var serr error
		if cfg.TLS.Enabled {
			serr = httpSrv.ServeTLS(ln, cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			serr = httpSrv.Serve(ln)
		}
		if !errors.Is(serr, http.ErrServerClosed) {
			errCh <- serr
		}
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		o.Log.Info("origin stopped")
		return nil
	case err := <-errCh:
		return err
	}
}

func deriveLocalURL(listen string, tls bool) string {
	scheme := "http"
	if tls {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return scheme + "://127.0.0.1" + listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s://%s:%s", scheme, host, port)
}
