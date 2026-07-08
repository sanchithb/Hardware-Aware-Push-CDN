// Package edge wires a runnable edge node: SIEVE content cache, data-plane
// HTTP server, controller enrollment and the heartbeat/directive loop.
package edge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/edge/cache"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/edge/serve"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/nodeagent"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/config"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/metrics"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/tlsutil"
)

// Edge is a runnable edge node.
type Edge struct {
	Cfg config.Edge
	Log *slog.Logger
}

// Run enrolls with the controller and serves until ctx is cancelled.
func (e *Edge) Run(ctx context.Context) error {
	cfg := e.Cfg
	if cfg.CacheDir == "" {
		cfg.CacheDir = cfg.DataDir + string(os.PathSeparator) + "cache"
	}
	if cfg.Name == "" {
		if hn, err := os.Hostname(); err == nil {
			cfg.Name = hn
		}
	}
	if cfg.PublicURL == "" {
		cfg.PublicURL = deriveLocalURL(cfg.Listen, cfg.TLS.Enabled)
		e.Log.Warn("public_url not set; derived for single-machine use", "public_url", cfg.PublicURL)
	}

	c, err := cache.Open(cfg.CacheDir, cfg.CacheMaxBytes, e.Log)
	if err != nil {
		return err
	}

	srv := &serve.Server{
		Cache:         c,
		Log:           e.Log,
		Metrics:       metrics.NewRegistry(),
		ControllerURL: strings.TrimRight(cfg.ControllerURL, "/"),
		Rewrite:       serve.RewriteController,
		HoldTimeout:   cfg.HoldTimeout,
		PlaylistTTL:   cfg.PlaylistTTL,
		SegmentTTL:    cfg.SegmentTTL,
		FetchClient:   nodeagent.NewHTTPClient(cfg.InsecureSkipVerify, 30*time.Second),
	}

	agent := &nodeagent.Agent{
		ControllerURL: strings.TrimRight(cfg.ControllerURL, "/"),
		Kind:          protocol.KindEdge,
		Name:          cfg.Name,
		PublicURL:     strings.TrimRight(cfg.PublicURL, "/"),
		Region:        cfg.Region,
		Capacity:      cfg.Capacity,
		DataDir:       cfg.DataDir,
		CacheDir:      cfg.CacheDir,
		Log:           e.Log,
		Stats:         srv,
		HTTP:          nodeagent.NewHTTPClient(cfg.InsecureSkipVerify, 10*time.Second),
		OnTopology: func(_ []protocol.EdgeEndpoint, origins []protocol.OriginEndpoint) {
			srv.SetOrigins(origins)
		},
		OnDirective: func(d protocol.Directive) {
			switch d.Type {
			case protocol.DirectivePurge:
				n := c.Purge(strings.TrimPrefix(d.Path, "/"))
				e.Log.Info("purge directive applied", "path", d.Path, "objects", n)
			case protocol.DirectiveDrain:
				srv.SetDraining(true)
				e.Log.Info("drain directive: refusing new sessions")
			case protocol.DirectiveUndrain:
				srv.SetDraining(false)
				e.Log.Info("undrain directive: accepting sessions")
			}
		},
	}

	if err := agent.Enroll(ctx, cfg.JoinToken); err != nil {
		return err
	}
	id := agent.Identity()
	srv.SigningKey = id.SigningKey
	srv.IngestKey = id.IngestKey

	mux := http.NewServeMux()
	srv.Mount(mux)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("edge: listen %s: %w", cfg.Listen, err)
	}
	if cfg.TLS.Enabled && cfg.TLS.CertFile == "" {
		tc, terr := tlsutil.SelfSigned()
		if terr != nil {
			return terr
		}
		httpSrv.TLSConfig = tc
	}

	go agent.RunHeartbeats(ctx)

	e.Log.Info("edge node serving",
		"addr", ln.Addr().String(), "node_id", id.NodeID,
		"cache_dir", cfg.CacheDir, "cache_max_bytes", cfg.CacheMaxBytes)

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
		e.Log.Info("edge stopped")
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
