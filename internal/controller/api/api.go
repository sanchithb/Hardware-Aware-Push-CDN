// Package api implements the controller's HTTP surface:
//
//	Public:   /play/* (signed playback routing), /healthz, /metrics
//	Node:     POST /api/v1/nodes/register, POST /api/v1/nodes/{id}/heartbeat
//	Admin:    everything else under /api/v1 (Bearer admin key)
//	Console:  / (embedded web console, wired in the controller package)
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/events"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/registry"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/router"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/store"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/auth"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/logx"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/metrics"
)

// Server bundles the controller's HTTP dependencies.
type Server struct {
	Registry     *registry.Registry
	Router       *router.Router
	Store        *store.Store
	Hub          *events.Hub
	Log          *slog.Logger
	LogRing      *logx.Ring
	Metrics      *metrics.Registry
	AdminKeyHash string
	RoutingMode  string // "redirect" | "proxy"

	routeTotal *metrics.Counter
	routeErrs  *metrics.Counter
	routeDur   *metrics.Histogram
	authFails  *metrics.Counter
}

type errBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errBody{Error: msg})
}

// Mount registers all controller routes onto mux.
func (s *Server) Mount(mux *http.ServeMux) {
	s.routeTotal = s.Metrics.Counter("hpcdn_route_decisions_total", "Playback routing decisions", nil)
	s.routeErrs = s.Metrics.Counter("hpcdn_route_errors_total", "Playback routing failures", nil)
	s.routeDur = s.Metrics.Histogram("hpcdn_route_duration_seconds", "Routing decision latency", nil)
	s.authFails = s.Metrics.Counter("hpcdn_auth_failures_total", "Rejected API or playback auth attempts", nil)

	// Public
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /metrics", s.Metrics.Handler())
	mux.HandleFunc("/play/", s.handlePlay)

	// Node endpoints
	mux.HandleFunc("POST /api/v1/nodes/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/nodes/{id}/heartbeat", s.handleHeartbeat)

	// Admin endpoints
	admin := func(h http.HandlerFunc) http.HandlerFunc { return s.requireAdmin(h) }
	mux.HandleFunc("GET /api/v1/stats", admin(s.handleStats))
	mux.HandleFunc("GET /api/v1/nodes", admin(s.handleNodes))
	mux.HandleFunc("GET /api/v1/nodes/{id}", admin(s.handleNode))
	mux.HandleFunc("GET /api/v1/nodes/{id}/telemetry", admin(s.handleTelemetry))
	mux.HandleFunc("POST /api/v1/nodes/{id}/drain", admin(s.handleDrain(true)))
	mux.HandleFunc("POST /api/v1/nodes/{id}/undrain", admin(s.handleDrain(false)))
	mux.HandleFunc("DELETE /api/v1/nodes/{id}", admin(s.handleRemoveNode))
	mux.HandleFunc("GET /api/v1/settings", admin(s.handleGetSettings))
	mux.HandleFunc("PUT /api/v1/settings", admin(s.handlePutSettings))
	mux.HandleFunc("POST /api/v1/tokens", admin(s.handleCreateToken))
	mux.HandleFunc("GET /api/v1/tokens", admin(s.handleListTokens))
	mux.HandleFunc("DELETE /api/v1/tokens/{id}", admin(s.handleDeleteToken))
	mux.HandleFunc("POST /api/v1/sign", admin(s.handleSign))
	mux.HandleFunc("POST /api/v1/purge", admin(s.handlePurge))
	mux.HandleFunc("GET /api/v1/events", admin(s.handleEvents))
	mux.HandleFunc("GET /api/v1/logs", admin(s.handleLogs))
	mux.HandleFunc("GET /api/v1/whoami", admin(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"role": "admin"})
	}))
}

// requireAdmin enforces the Bearer admin key with a constant-time compare.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		key, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(store.Hash(key)), []byte(s.AdminKeyHash)) != 1 {
			s.authFails.Inc()
			writeErr(w, http.StatusUnauthorized, "missing or invalid admin API key")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req protocol.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	resp, err := s.Registry.Register(req)
	if err != nil {
		if errors.Is(err, registry.ErrBadToken) {
			s.authFails.Inc()
			writeErr(w, http.StatusUnauthorized, "invalid or exhausted join token")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	secret := r.Header.Get(protocol.HeaderNodeSecret)
	var hb protocol.Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	resp, err := s.Registry.Heartbeat(id, secret, hb)
	if err != nil {
		s.authFails.Inc()
		writeErr(w, http.StatusUnauthorized, "heartbeat rejected")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Registry.Stats())
}

func (s *Server) handleNodes(w http.ResponseWriter, _ *http.Request) {
	nodes := s.Registry.Nodes()
	if nodes == nil {
		nodes = []protocol.NodeStatus{}
	}
	writeJSON(w, http.StatusOK, nodes)
}

func (s *Server) handleNode(w http.ResponseWriter, r *http.Request) {
	st, err := s.Registry.Node(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	hist, err := s.Registry.Telemetry(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	if hist == nil {
		hist = []protocol.TelemetrySample{}
	}
	writeJSON(w, http.StatusOK, hist)
}

func (s *Server) handleDrain(drain bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.Registry.SetDrain(r.PathValue("id"), drain); err != nil {
			writeErr(w, http.StatusNotFound, "node not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"draining": drain})
	}
}

func (s *Server) handleRemoveNode(w http.ResponseWriter, r *http.Request) {
	if err := s.Registry.Remove(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Registry.Settings())
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in protocol.Settings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.Registry.UpdateSettings(in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Note       string `json:"note"`
		TTLSeconds int    `json:"ttl_seconds"`
		MaxUses    int    `json:"max_uses"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // empty body = defaults
	}
	info, err := s.Registry.CreateToken(req.Note, time.Duration(req.TTLSeconds)*time.Second, req.MaxUses)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) handleListTokens(w http.ResponseWriter, _ *http.Request) {
	toks := s.Registry.ListTokens()
	if toks == nil {
		toks = []protocol.JoinTokenInfo{}
	}
	writeJSON(w, http.StatusOK, toks)
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	if err := s.Registry.DeleteToken(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) signingKey() string {
	var k string
	s.Store.View(func(st *store.State) { k = st.SigningKey })
	return k
}

func (s *Server) handleSign(w http.ResponseWriter, r *http.Request) {
	var req protocol.SignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeErr(w, http.StatusBadRequest, "body must include path")
		return
	}
	if !strings.HasPrefix(req.Path, "/play/") {
		req.Path = "/play/" + strings.TrimPrefix(req.Path, "/")
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = time.Duration(s.Registry.Settings().SignTTLSeconds) * time.Second
	}
	scope := req.Scope
	if scope == "" {
		// Default scope: the stream directory, so the one token covers the
		// playlist and every segment it references.
		if i := strings.LastIndex(req.Path, "/"); i > len("/play") {
			scope = req.Path[:i+1]
		}
	}
	q := auth.SignPath(s.signingKey(), req.Path, scope, ttl)
	writeJSON(w, http.StatusOK, protocol.SignResponse{
		URL:       req.Path + "?" + q,
		ExpiresAt: time.Now().Add(ttl).UTC(),
	})
}

func (s *Server) handlePurge(w http.ResponseWriter, r *http.Request) {
	var req protocol.PurgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeErr(w, http.StatusBadRequest, "body must include path")
		return
	}
	n := s.Registry.Purge(req.Path)
	writeJSON(w, http.StatusOK, map[string]int{"edges_notified": n})
}

func (s *Server) handleLogs(w http.ResponseWriter, _ *http.Request) {
	entries := s.LogRing.Snapshot()
	if entries == nil {
		entries = []logx.Entry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleEvents streams controller events as Server-Sent Events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, replay, cancel := s.Hub.Subscribe()
	defer cancel()

	send := func(ev protocol.Event) bool {
		b, err := json.Marshal(ev)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	for _, ev := range replay {
		if !send(ev) {
			return
		}
	}
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if !send(ev) {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// handlePlay validates the playback signature, picks an edge and either
// 302-redirects the player there or reverse-proxies the stream.
func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if err := auth.ValidateQuery(s.signingKey(), r.URL.Path, r.URL.Query(), time.Now()); err != nil {
		s.authFails.Inc()
		s.Log.Warn("playback auth rejected", "path", r.URL.Path, "reason", err)
		writeErr(w, http.StatusForbidden, "invalid or expired stream signature")
		return
	}

	// Stream key = first path segment under /play → cache affinity per stream.
	rest := strings.TrimPrefix(r.URL.Path, "/play/")
	streamKey := rest
	if i := strings.IndexByte(rest, '/'); i > 0 {
		streamKey = rest[:i]
	}
	region := r.Header.Get("X-User-Region")

	dec, err := s.Router.Pick(s.Registry.Candidates(), streamKey, region, s.Registry.Settings())
	s.routeDur.Observe(time.Since(start).Seconds())
	if err != nil {
		s.routeErrs.Inc()
		writeErr(w, http.StatusServiceUnavailable, "no edge nodes available")
		return
	}
	s.routeTotal.Inc()
	s.Registry.RecordRoute(dec.NodeID)

	target := dec.PublicURL + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	if s.RoutingMode == "proxy" {
		u, perr := url.Parse(dec.PublicURL)
		if perr != nil {
			writeErr(w, http.StatusBadGateway, "bad edge URL")
			return
		}
		proxy := httputil.NewSingleHostReverseProxy(u)
		proxy.ErrorHandler = func(pw http.ResponseWriter, _ *http.Request, perr error) {
			s.Log.Warn("proxy to edge failed", "edge", dec.NodeID, "err", perr)
			writeErr(pw, http.StatusBadGateway, "edge unreachable")
		}
		proxy.ServeHTTP(w, r)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}
