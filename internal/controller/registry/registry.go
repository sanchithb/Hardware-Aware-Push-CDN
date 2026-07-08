// Package registry is the controller's authoritative view of the cluster:
// node enrollment via join tokens, heartbeat ingestion with EWMA-smoothed
// load scores, per-node telemetry history for the console, a directive
// queue (purge/drain) with at-least-once delivery, and an active health
// prober implementing an Envoy-style outlier ejection circuit breaker
// with a panic threshold.
package registry

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/events"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/store"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/auth"
)

// Enrollment / lookup errors.
var (
	ErrBadToken     = errors.New("registry: invalid or exhausted join token")
	ErrUnknownNode  = errors.New("registry: unknown node")
	ErrUnauthorized = errors.New("registry: node secret mismatch")
)

// node is the in-memory record for one enrolled node.
type node struct {
	store.PersistedNode

	lastSeen  time.Time
	firstSeen time.Time // this process; for rate warm-up
	draining  bool

	hb       protocol.Heartbeat
	ewma     float64 // smoothed composite load score, 0..100+
	outRate  float64 // bytes/sec derived from successive heartbeats
	inRate   float64
	prevHB   protocol.Heartbeat
	prevAt   time.Time
	history  []protocol.TelemetrySample // ring, newest at end
	routed   int64
	routedAt []time.Time // recent route timestamps for req/s (trimmed)

	// circuit breaker driven by the active prober
	ejected      bool
	probeFails   int
	halfOpenAt   time.Time
	dirSeq       uint64
	dirQueue     []protocol.Directive
	secretHashed string
}

// Config tunes the registry.
type Config struct {
	HeartbeatInterval time.Duration
	HistorySize       int
	ProbeInterval     time.Duration
	ProbeTimeout      time.Duration
	ProbeFailures     int           // consecutive failures before ejection
	EjectCooldown     time.Duration // before a half-open probe
}

// DefaultConfig returns production-reasonable registry tuning.
func DefaultConfig() Config {
	return Config{
		HeartbeatInterval: 2 * time.Second,
		HistorySize:       360,
		ProbeInterval:     5 * time.Second,
		ProbeTimeout:      2 * time.Second,
		ProbeFailures:     3,
		EjectCooldown:     15 * time.Second,
	}
}

// Registry holds all nodes and cluster settings.
type Registry struct {
	mu       sync.RWMutex
	nodes    map[string]*node
	settings protocol.Settings
	cfg      Config
	st       *store.Store
	hub      *events.Hub
	log      *slog.Logger
	started  time.Time
	probe    *http.Client
}

// New builds a Registry, restoring enrolled nodes from the store.
func New(st *store.Store, hub *events.Hub, cfg Config, log *slog.Logger) *Registry {
	r := &Registry{
		nodes:   map[string]*node{},
		cfg:     cfg,
		st:      st,
		hub:     hub,
		log:     log,
		started: time.Now(),
		probe:   &http.Client{Timeout: cfg.ProbeTimeout},
	}
	st.View(func(s *store.State) {
		r.settings = s.Settings
		for _, pn := range s.Nodes {
			r.nodes[pn.ID] = &node{PersistedNode: pn, secretHashed: pn.SecretHash}
		}
	})
	return r
}

// Settings returns the current routing settings.
func (r *Registry) Settings() protocol.Settings {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.settings
}

// UpdateSettings persists and applies new settings.
func (r *Registry) UpdateSettings(s protocol.Settings) error {
	if s.EWMAAlpha <= 0 || s.EWMAAlpha > 1 {
		return fmt.Errorf("ewma_alpha must be in (0,1]")
	}
	if s.HeartbeatTTL <= 0 || s.SignTTLSeconds <= 0 {
		return fmt.Errorf("heartbeat_ttl_seconds and sign_ttl_seconds must be positive")
	}
	if err := r.st.Mutate(func(st *store.State) { st.Settings = s }); err != nil {
		return err
	}
	r.mu.Lock()
	r.settings = s
	r.mu.Unlock()
	r.hub.Publish("settings", "", "routing settings updated")
	return nil
}

// CreateToken mints a join token. ttl<=0 means no expiry; maxUses<=0 means
// unlimited. Returns the plaintext token exactly once.
func (r *Registry) CreateToken(note string, ttl time.Duration, maxUses int) (protocol.JoinTokenInfo, error) {
	tok := auth.NewToken(auth.PrefixJoinToken)
	info := protocol.JoinTokenInfo{
		ID:        auth.NewToken("jt_")[:11], // short random id
		Token:     tok,
		Note:      note,
		CreatedAt: time.Now().UTC(),
		MaxUses:   maxUses,
	}
	rec := store.JoinToken{
		ID: info.ID, TokenHash: store.Hash(tok), Note: note,
		CreatedAt: info.CreatedAt, MaxUses: maxUses,
	}
	if ttl > 0 {
		exp := info.CreatedAt.Add(ttl)
		info.ExpiresAt = &exp
		rec.ExpiresAt = &exp
	}
	if err := r.st.Mutate(func(s *store.State) { s.Tokens = append(s.Tokens, rec) }); err != nil {
		return protocol.JoinTokenInfo{}, err
	}
	return info, nil
}

// ListTokens returns token metadata (never plaintext).
func (r *Registry) ListTokens() []protocol.JoinTokenInfo {
	var out []protocol.JoinTokenInfo
	r.st.View(func(s *store.State) {
		for _, t := range s.Tokens {
			out = append(out, protocol.JoinTokenInfo{
				ID: t.ID, Note: t.Note, CreatedAt: t.CreatedAt,
				ExpiresAt: t.ExpiresAt, MaxUses: t.MaxUses, Uses: t.Uses,
			})
		}
	})
	return out
}

// DeleteToken revokes a join token by id.
func (r *Registry) DeleteToken(id string) error {
	found := false
	err := r.st.Mutate(func(s *store.State) {
		out := s.Tokens[:0]
		for _, t := range s.Tokens {
			if t.ID == id {
				found = true
				continue
			}
			out = append(out, t)
		}
		s.Tokens = out
	})
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("token %q not found", id)
	}
	return nil
}

// Register enrolls a node. The join token is validated against stored
// hashes, use counts are enforced, and a fresh node identity is issued.
func (r *Registry) Register(req protocol.RegisterRequest) (protocol.RegisterResponse, error) {
	if req.Kind != protocol.KindEdge && req.Kind != protocol.KindOrigin {
		return protocol.RegisterResponse{}, fmt.Errorf("registry: invalid node kind %q", req.Kind)
	}
	if req.PublicURL == "" {
		return protocol.RegisterResponse{}, fmt.Errorf("registry: public_url is required")
	}
	tokenHash := store.Hash(req.JoinToken)
	valid := false
	var signingKey, ingestKey string
	err := r.st.Mutate(func(s *store.State) {
		for i := range s.Tokens {
			t := &s.Tokens[i]
			if subtle.ConstantTimeCompare([]byte(t.TokenHash), []byte(tokenHash)) != 1 {
				continue
			}
			if t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt) {
				return
			}
			if t.MaxUses > 0 && t.Uses >= t.MaxUses {
				return
			}
			t.Uses++
			valid = true
			signingKey, ingestKey = s.SigningKey, s.IngestKey
			return
		}
	})
	if err != nil {
		return protocol.RegisterResponse{}, err
	}
	if !valid {
		return protocol.RegisterResponse{}, ErrBadToken
	}

	secret := auth.NewToken(auth.PrefixNodeSecret)
	n := &node{
		PersistedNode: store.PersistedNode{
			ID:         auth.NewToken("nd_")[:11],
			Kind:       req.Kind,
			Name:       req.Name,
			PublicURL:  strings.TrimRight(req.PublicURL, "/"),
			Region:     req.Region,
			Capacity:   req.Capacity,
			SecretHash: store.Hash(secret),
			JoinedAt:   time.Now().UTC(),
		},
		secretHashed: store.Hash(secret),
		lastSeen:     time.Now(),
		firstSeen:    time.Now(),
	}
	if n.Capacity <= 0 {
		n.Capacity = 1000
	}
	if n.Name == "" {
		n.Name = string(req.Kind) + "-" + n.ID[3:]
	}

	if err := r.st.Mutate(func(s *store.State) { s.Nodes = append(s.Nodes, n.PersistedNode) }); err != nil {
		return protocol.RegisterResponse{}, err
	}
	r.mu.Lock()
	r.nodes[n.ID] = n
	r.mu.Unlock()

	r.log.Info("node enrolled", "id", n.ID, "kind", n.Kind, "name", n.Name, "region", n.Region, "url", n.PublicURL)
	r.hub.Publish("node_joined", n.ID, fmt.Sprintf("%s %q joined (%s)", n.Kind, n.Name, n.PublicURL))

	return protocol.RegisterResponse{
		NodeID:            n.ID,
		NodeSecret:        secret,
		SigningKey:        signingKey,
		IngestKey:         ingestKey,
		HeartbeatInterval: int(r.cfg.HeartbeatInterval / time.Second),
	}, nil
}

// Authenticate verifies a node id + secret pair.
func (r *Registry) Authenticate(id, secret string) (*node, error) {
	r.mu.RLock()
	n, ok := r.nodes[id]
	r.mu.RUnlock()
	if !ok {
		return nil, ErrUnknownNode
	}
	if subtle.ConstantTimeCompare([]byte(n.secretHashed), []byte(store.Hash(secret))) != 1 {
		return nil, ErrUnauthorized
	}
	return n, nil
}

// Heartbeat ingests telemetry from an authenticated node and returns the
// directives and topology for its role.
func (r *Registry) Heartbeat(id, secret string, hb protocol.Heartbeat) (protocol.HeartbeatResponse, error) {
	n, err := r.Authenticate(id, secret)
	if err != nil {
		return protocol.HeartbeatResponse{}, err
	}

	r.mu.Lock()
	now := time.Now()
	wasOffline := !n.lastSeen.IsZero() && now.Sub(n.lastSeen) > r.ttl()
	if n.firstSeen.IsZero() {
		n.firstSeen = now
	}

	// Derive byte rates from successive cumulative counters.
	if !n.prevAt.IsZero() {
		dt := now.Sub(n.prevAt).Seconds()
		if dt > 0 && hb.BytesOut >= n.prevHB.BytesOut {
			n.outRate = float64(hb.BytesOut-n.prevHB.BytesOut) / dt
		}
		if dt > 0 && hb.BytesIn >= n.prevHB.BytesIn {
			n.inRate = float64(hb.BytesIn-n.prevHB.BytesIn) / dt
		}
	}
	n.prevHB, n.prevAt = hb, now

	// EWMA-smooth the composite hardware score.
	s := r.settings
	raw := hb.CPUPercent*s.CPUWeight + hb.RAMPercent*s.RAMWeight
	if n.Capacity > 0 {
		raw += float64(hb.ActiveConns) / float64(n.Capacity) * 100 * s.ConnWeight
	}
	if n.ewma == 0 {
		n.ewma = raw
	} else {
		n.ewma = s.EWMAAlpha*raw + (1-s.EWMAAlpha)*n.ewma
	}

	n.hb = hb
	n.lastSeen = now

	// Telemetry history ring for console charts.
	sample := protocol.TelemetrySample{
		T: now.UTC(), CPU: hb.CPUPercent, RAM: hb.RAMPercent, Disk: hb.DiskPercent,
		Conns: hb.ActiveConns, BytesOutRate: n.outRate, BytesInRate: n.inRate,
		HitRatio: hitRatio(hb.CacheHits, hb.CacheMisses),
	}
	n.history = append(n.history, sample)
	if len(n.history) > r.cfg.HistorySize {
		n.history = n.history[len(n.history)-r.cfg.HistorySize:]
	}

	// Directive delivery: drop acknowledged, send the rest.
	if hb.AckSeq > 0 {
		q := n.dirQueue[:0]
		for _, d := range n.dirQueue {
			if d.Seq > hb.AckSeq {
				q = append(q, d)
			}
		}
		n.dirQueue = q
	}
	resp := protocol.HeartbeatResponse{Directives: append([]protocol.Directive(nil), n.dirQueue...)}

	// Topology exchange: origins learn edges, edges learn origins.
	switch n.Kind {
	case protocol.KindOrigin:
		for _, e := range r.aliveLocked(protocol.KindEdge) {
			resp.Edges = append(resp.Edges, protocol.EdgeEndpoint{NodeID: e.ID, IngestURL: e.PublicURL + "/ingest"})
		}
	case protocol.KindEdge:
		for _, o := range r.aliveLocked(protocol.KindOrigin) {
			resp.Origins = append(resp.Origins, protocol.OriginEndpoint{NodeID: o.ID, FetchURL: o.PublicURL + "/fetch"})
		}
	}
	r.mu.Unlock()

	if wasOffline {
		r.hub.Publish("node_recovered", n.ID, fmt.Sprintf("%s %q is back online", n.Kind, n.Name))
	}
	return resp, nil
}

func hitRatio(hits, misses uint64) float64 {
	if hits+misses == 0 {
		return 0
	}
	return float64(hits) / float64(hits+misses)
}

func (r *Registry) ttl() time.Duration {
	ttl := time.Duration(r.settings.HeartbeatTTL) * time.Second
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	return ttl
}

// aliveLocked returns nodes of a kind seen within the heartbeat TTL.
// Callers must hold r.mu (read or write).
func (r *Registry) aliveLocked(kind protocol.NodeKind) []*node {
	var out []*node
	for _, n := range r.nodes {
		if n.Kind == kind && time.Since(n.lastSeen) <= r.ttl() && !n.draining {
			out = append(out, n)
		}
	}
	return out
}

// stateOf derives the externally visible state string.
func (r *Registry) stateOf(n *node) string {
	switch {
	case n.lastSeen.IsZero() || time.Since(n.lastSeen) > r.ttl():
		return "offline"
	case n.ejected:
		return "ejected"
	case n.draining:
		return "draining"
	case time.Since(n.lastSeen) > 2*r.cfg.HeartbeatInterval+time.Second:
		return "degraded"
	default:
		return "healthy"
	}
}

func (r *Registry) statusLocked(n *node) protocol.NodeStatus {
	return protocol.NodeStatus{
		ID: n.ID, Kind: n.Kind, Name: n.Name, PublicURL: n.PublicURL,
		Region: n.Region, Capacity: n.Capacity, Version: n.hb.Version,
		State: r.stateOf(n), Draining: n.draining,
		LastSeen: n.lastSeen.UTC(), JoinedAt: n.JoinedAt,
		Score: n.ewma, CPUPercent: n.hb.CPUPercent, RAMPercent: n.hb.RAMPercent,
		DiskPercent: n.hb.DiskPercent, ActiveConns: n.hb.ActiveConns,
		BytesOutRate: n.outRate, BytesInRate: n.inRate,
		HitRatio:   hitRatio(n.hb.CacheHits, n.hb.CacheMisses),
		CacheBytes: n.hb.CacheBytes, CacheFiles: n.hb.CacheFiles,
		UptimeSeconds: n.hb.UptimeSeconds, RoutedTotal: n.routed,
	}
}

// Nodes returns status views of every node, stably ordered by join time.
func (r *Registry) Nodes() []protocol.NodeStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]protocol.NodeStatus, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, r.statusLocked(n))
	}
	sortStatuses(out)
	return out
}

func sortStatuses(list []protocol.NodeStatus) {
	sort.Slice(list, func(i, j int) bool { return list[i].JoinedAt.Before(list[j].JoinedAt) })
}

// Node returns one node's status.
func (r *Registry) Node(id string) (protocol.NodeStatus, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[id]
	if !ok {
		return protocol.NodeStatus{}, ErrUnknownNode
	}
	return r.statusLocked(n), nil
}

// Telemetry returns a node's stored history samples.
func (r *Registry) Telemetry(id string) ([]protocol.TelemetrySample, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[id]
	if !ok {
		return nil, ErrUnknownNode
	}
	return append([]protocol.TelemetrySample(nil), n.history...), nil
}

// SetDrain marks a node as (un)draining and queues the directive so the
// node itself can stop/resume accepting new sessions.
func (r *Registry) SetDrain(id string, drain bool) error {
	r.mu.Lock()
	n, ok := r.nodes[id]
	if !ok {
		r.mu.Unlock()
		return ErrUnknownNode
	}
	n.draining = drain
	typ := protocol.DirectiveDrain
	if !drain {
		typ = protocol.DirectiveUndrain
	}
	n.dirSeq++
	n.dirQueue = append(n.dirQueue, protocol.Directive{Seq: n.dirSeq, Type: typ})
	name := n.Name
	r.mu.Unlock()
	verb := "draining"
	if !drain {
		verb = "resumed"
	}
	r.hub.Publish("node_drain", id, fmt.Sprintf("node %q %s", name, verb))
	return nil
}

// Remove deregisters a node permanently.
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	n, ok := r.nodes[id]
	if ok {
		delete(r.nodes, id)
	}
	r.mu.Unlock()
	if !ok {
		return ErrUnknownNode
	}
	err := r.st.Mutate(func(s *store.State) {
		out := s.Nodes[:0]
		for _, pn := range s.Nodes {
			if pn.ID != id {
				out = append(out, pn)
			}
		}
		s.Nodes = out
	})
	r.hub.Publish("node_removed", id, fmt.Sprintf("node %q removed", n.Name))
	return err
}

// Purge queues a purge directive for every edge node.
func (r *Registry) Purge(pathPrefix string) int {
	r.mu.Lock()
	count := 0
	for _, n := range r.nodes {
		if n.Kind != protocol.KindEdge {
			continue
		}
		n.dirSeq++
		n.dirQueue = append(n.dirQueue, protocol.Directive{Seq: n.dirSeq, Type: protocol.DirectivePurge, Path: pathPrefix})
		count++
	}
	r.mu.Unlock()
	r.hub.Publish("purge", "", fmt.Sprintf("purge %q queued to %d edge(s)", pathPrefix, count))
	return count
}

// RecordRoute counts a routing decision toward a node (for stats).
func (r *Registry) RecordRoute(id string) {
	r.mu.Lock()
	if n, ok := r.nodes[id]; ok {
		n.routed++
		n.routedAt = append(n.routedAt, time.Now())
		if len(n.routedAt) > 512 {
			n.routedAt = n.routedAt[len(n.routedAt)-512:]
		}
	}
	r.mu.Unlock()
}

// Stats aggregates the cluster overview.
func (r *Registry) Stats() protocol.ClusterStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st := protocol.ClusterStats{UptimeSeconds: int64(time.Since(r.started).Seconds())}
	var hits, misses uint64
	cutoff := time.Now().Add(-10 * time.Second)
	recent := 0
	for _, n := range r.nodes {
		st.Nodes++
		st.RoutedTotal += n.routed
		for i := len(n.routedAt) - 1; i >= 0; i-- {
			if n.routedAt[i].After(cutoff) {
				recent++
			} else {
				break
			}
		}
		switch n.Kind {
		case protocol.KindEdge:
			st.EdgesTotal++
			if r.stateOf(n) == "healthy" {
				st.EdgesHealthy++
			}
			st.BytesOutRate += n.outRate
			st.CacheBytes += n.hb.CacheBytes
			hits += n.hb.CacheHits
			misses += n.hb.CacheMisses
		case protocol.KindOrigin:
			st.OriginsTotal++
		}
	}
	st.RoutedPerSec = float64(recent) / 10
	st.HitRatio = hitRatio(hits, misses)
	return st
}

// RouteCandidate is the router's view of an edge node.
type RouteCandidate struct {
	ID        string
	PublicURL string
	Region    string
	Score     float64 // EWMA composite, before region penalty
	Draining  bool
	Ejected   bool
	Healthy   bool
}

// Candidates returns live edge nodes for routing. It includes ejected
// nodes flagged so the router can implement the panic threshold.
func (r *Registry) Candidates() []RouteCandidate {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []RouteCandidate
	for _, n := range r.nodes {
		if n.Kind != protocol.KindEdge {
			continue
		}
		alive := !n.lastSeen.IsZero() && time.Since(n.lastSeen) <= r.ttl()
		out = append(out, RouteCandidate{
			ID: n.ID, PublicURL: n.PublicURL, Region: n.Region,
			Score: n.ewma, Draining: n.draining, Ejected: n.ejected,
			Healthy: alive,
		})
	}
	return out
}

// StartProber launches the active health prober loop. Each cycle it GETs
// every live edge's /healthz; ProbeFailures consecutive failures ejects
// the node (circuit open). After EjectCooldown a success re-admits it
// (half-open probe succeeds → circuit closes).
func (r *Registry) StartProber(ctx context.Context) {
	go func() {
		t := time.NewTicker(r.cfg.ProbeInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.probeOnce(ctx)
			}
		}
	}()
}

func (r *Registry) probeOnce(ctx context.Context) {
	r.mu.RLock()
	targets := make([]*node, 0, len(r.nodes))
	for _, n := range r.nodes {
		if n.Kind == protocol.KindEdge && !n.lastSeen.IsZero() && time.Since(n.lastSeen) <= r.ttl() {
			if n.ejected && time.Now().Before(n.halfOpenAt) {
				continue // still cooling down
			}
			targets = append(targets, n)
		}
	}
	r.mu.RUnlock()

	var wg sync.WaitGroup
	for _, n := range targets {
		wg.Add(1)
		go func(n *node) {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.PublicURL+"/healthz", nil)
			if err != nil {
				return
			}
			resp, err := r.probe.Do(req)
			ok := err == nil && resp.StatusCode == http.StatusOK
			if resp != nil {
				resp.Body.Close()
			}
			r.mu.Lock()
			if ok {
				if n.ejected {
					n.ejected = false
					r.mu.Unlock()
					r.log.Info("circuit closed, node re-admitted", "id", n.ID, "name", n.Name)
					r.hub.Publish("node_recovered", n.ID, fmt.Sprintf("node %q passed half-open probe, re-admitted", n.Name))
					r.mu.Lock()
				}
				n.probeFails = 0
			} else {
				n.probeFails++
				if !n.ejected && n.probeFails >= r.cfg.ProbeFailures {
					n.ejected = true
					n.halfOpenAt = time.Now().Add(r.cfg.EjectCooldown)
					r.mu.Unlock()
					r.log.Warn("circuit opened, node ejected", "id", n.ID, "name", n.Name, "fails", n.probeFails)
					r.hub.Publish("node_ejected", n.ID, fmt.Sprintf("node %q ejected after %d failed probes", n.Name, n.probeFails))
					r.mu.Lock()
				} else if n.ejected {
					n.halfOpenAt = time.Now().Add(r.cfg.EjectCooldown)
				}
			}
			r.mu.Unlock()
		}(n)
	}
	wg.Wait()
}
