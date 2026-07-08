// Package controller wires the hpcdn control plane: state store, node
// registry, health prober, routing engine, admin/node HTTP API, metrics
// and the embedded web console.
package controller

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/api"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/events"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/registry"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/router"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/store"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/auth"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/config"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/logx"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/metrics"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/tlsutil"
)

// Controller is a runnable control-plane instance.
type Controller struct {
	Cfg     config.Controller
	Log     *slog.Logger
	LogRing *logx.Ring

	store *store.Store
	reg   *registry.Registry
	hub   *events.Hub
	srv   *http.Server

	// ConsoleFS, when set, is served at / (the embedded web console).
	ConsoleFS fs.FS
	// AdminKey is the plaintext admin key if it was generated this boot;
	// empty when a pre-existing key hash was found. Used to print it once.
	AdminKey string
}

// New prepares a controller (no listening yet).
func New(cfg config.Controller, log *slog.Logger, ring *logx.Ring) (*Controller, error) {
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}

	c := &Controller{Cfg: cfg, Log: log, LogRing: ring, store: st}

	// Bootstrap cluster credentials on first boot.
	err = st.Mutate(func(s *store.State) {
		if s.SigningKey == "" {
			s.SigningKey = auth.NewToken(auth.PrefixSigningKey)
		}
		if s.IngestKey == "" {
			s.IngestKey = auth.NewToken(auth.PrefixIngestKey)
		}
		if cfg.AdminKey != "" {
			s.AdminKeyHash = store.Hash(cfg.AdminKey)
		} else if s.AdminKeyHash == "" {
			c.AdminKey = auth.NewToken(auth.PrefixAdminKey)
			s.AdminKeyHash = store.Hash(c.AdminKey)
		}
	})
	if err != nil {
		return nil, err
	}

	// Convenience for same-machine CLI use: persist the generated key with
	// owner-only permissions so `hpcdn` commands can find it automatically.
	if c.AdminKey != "" {
		keyPath := filepath.Join(cfg.DataDir, "admin.key")
		if werr := os.WriteFile(keyPath, []byte(c.AdminKey+"\n"), 0o600); werr != nil {
			log.Warn("could not persist admin key file", "path", keyPath, "err", werr)
		}
	}

	c.hub = events.NewHub(200)
	rcfg := registry.DefaultConfig()
	if cfg.HeartbeatInterval > 0 {
		rcfg.HeartbeatInterval = time.Duration(cfg.HeartbeatInterval) * time.Second
	}
	if cfg.TelemetryHistory > 0 {
		rcfg.HistorySize = cfg.TelemetryHistory
	}
	c.reg = registry.New(st, c.hub, rcfg, log)
	return c, nil
}

// AdminKeyHash exposes the stored hash (for the API layer).
func (c *Controller) adminKeyHash() string {
	var h string
	c.store.View(func(s *store.State) { h = s.AdminKeyHash })
	return h
}

// Run starts the control plane and blocks until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	mreg := metrics.NewRegistry()
	mux := http.NewServeMux()

	apiSrv := &api.Server{
		Registry:     c.reg,
		Router:       router.New(),
		Store:        c.store,
		Hub:          c.hub,
		Log:          c.Log,
		LogRing:      c.LogRing,
		Metrics:      mreg,
		AdminKeyHash: c.adminKeyHash(),
		RoutingMode:  c.Cfg.RoutingMode,
	}
	apiSrv.Mount(mux)

	if c.ConsoleFS != nil {
		mux.Handle("/", http.FileServerFS(c.ConsoleFS))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintln(w, "hpcdn controller — console not embedded in this build")
		})
	}

	c.reg.StartProber(ctx)

	c.srv = &http.Server{
		Addr:              c.Cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", c.Cfg.Listen)
	if err != nil {
		return fmt.Errorf("controller: listen %s: %w", c.Cfg.Listen, err)
	}

	scheme := "http"
	if c.Cfg.TLS.Enabled {
		scheme = "https"
		if c.Cfg.TLS.CertFile != "" && c.Cfg.TLS.KeyFile != "" {
			// http.Server.ServeTLS loads the files.
		} else {
			tc, terr := tlsutil.SelfSigned(hostOf(c.Cfg.PublicURL))
			if terr != nil {
				return terr
			}
			c.srv.TLSConfig = tc
		}
	}

	c.Log.Info("controller listening",
		"addr", ln.Addr().String(), "scheme", scheme,
		"routing_mode", c.Cfg.RoutingMode, "data_dir", c.Cfg.DataDir)

	errCh := make(chan error, 1)
	go func() {
		var serr error
		if c.Cfg.TLS.Enabled {
			serr = c.srv.ServeTLS(ln, c.Cfg.TLS.CertFile, c.Cfg.TLS.KeyFile)
		} else {
			serr = c.srv.Serve(ln)
		}
		if !errors.Is(serr, http.ErrServerClosed) {
			errCh <- serr
		}
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.srv.Shutdown(shutCtx)
		c.Log.Info("controller stopped")
		return nil
	case err := <-errCh:
		return err
	}
}

func hostOf(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
