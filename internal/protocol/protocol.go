// Package protocol defines the wire types exchanged between the hpcdn
// control plane (controller) and data-plane nodes (edges and origins).
// Both sides import this package so requests and responses cannot drift.
package protocol

import "time"

// NodeKind identifies the role of a registered node.
type NodeKind string

// Node kinds.
const (
	KindEdge   NodeKind = "edge"
	KindOrigin NodeKind = "origin"
)

// Header names used on authenticated node → controller requests.
const (
	HeaderNodeID     = "X-HPCDN-Node-ID"
	HeaderNodeSecret = "X-HPCDN-Node-Secret"
	HeaderIngestKey  = "X-HPCDN-Ingest-Key"
	HeaderChecksum   = "X-HPCDN-SHA256"
)

// RegisterRequest enrolls a node with the controller using a join token.
type RegisterRequest struct {
	JoinToken string   `json:"join_token"`
	Kind      NodeKind `json:"kind"`
	Name      string   `json:"name"`
	PublicURL string   `json:"public_url"` // URL clients/peers use to reach this node
	Region    string   `json:"region"`
	Capacity  int      `json:"capacity_conns"` // soft concurrent-connection capacity
	Version   string   `json:"version"`
}

// RegisterResponse carries the node identity plus the cluster keys the
// node needs to participate: the URL-signing key (edges validate playback
// signatures locally, no per-request controller round-trip) and the
// ingest key (authenticates origin→edge pushes and edge→origin pulls).
type RegisterResponse struct {
	NodeID            string `json:"node_id"`
	NodeSecret        string `json:"node_secret"`
	SigningKey        string `json:"signing_key"`
	IngestKey         string `json:"ingest_key"`
	HeartbeatInterval int    `json:"heartbeat_interval_seconds"`
}

// Heartbeat is the periodic telemetry + liveness report from a node.
// Byte/hit counters are cumulative since process start; the controller
// derives rates from successive samples.
type Heartbeat struct {
	CPUPercent  float64 `json:"cpu_percent"`
	RAMPercent  float64 `json:"ram_percent"`
	DiskPercent float64 `json:"disk_percent"` // of the cache volume
	Goroutines  int     `json:"goroutines"`

	ActiveConns int    `json:"active_conns"`
	BytesOut    uint64 `json:"bytes_out"`
	BytesIn     uint64 `json:"bytes_in"`

	CacheHits   uint64 `json:"cache_hits"`
	CacheMisses uint64 `json:"cache_misses"`
	CacheBytes  uint64 `json:"cache_bytes"`
	CacheFiles  int    `json:"cache_files"`

	UptimeSeconds int64  `json:"uptime_seconds"`
	Version       string `json:"version"`

	// AckSeq acknowledges directives with Seq <= AckSeq so the controller
	// can drop them from this node's queue (at-least-once delivery).
	AckSeq uint64 `json:"ack_seq"`
}

// DirectiveType enumerates commands the controller can issue to nodes.
type DirectiveType string

// Directive types.
const (
	DirectivePurge   DirectiveType = "purge"   // remove cached content under Path
	DirectiveDrain   DirectiveType = "drain"   // stop accepting new sessions
	DirectiveUndrain DirectiveType = "undrain" // resume accepting sessions
)

// Directive is a queued command delivered to a node in heartbeat responses.
type Directive struct {
	Seq  uint64        `json:"seq"`
	Type DirectiveType `json:"type"`
	Path string        `json:"path,omitempty"` // for purge: path prefix
}

// EdgeEndpoint tells an origin where to push content.
type EdgeEndpoint struct {
	NodeID    string `json:"node_id"`
	IngestURL string `json:"ingest_url"`
}

// OriginEndpoint tells an edge where to pull on cache miss.
type OriginEndpoint struct {
	NodeID   string `json:"node_id"`
	FetchURL string `json:"fetch_url"`
}

// HeartbeatResponse is the controller's reply: pending directives plus the
// current peer topology relevant to the node's role.
type HeartbeatResponse struct {
	Directives []Directive      `json:"directives,omitempty"`
	Edges      []EdgeEndpoint   `json:"edges,omitempty"`   // sent to origins
	Origins    []OriginEndpoint `json:"origins,omitempty"` // sent to edges
}

// NodeStatus is the controller's externally visible view of a node,
// returned by the admin API and consumed by the CLI and web console.
type NodeStatus struct {
	ID        string    `json:"id"`
	Kind      NodeKind  `json:"kind"`
	Name      string    `json:"name"`
	PublicURL string    `json:"public_url"`
	Region    string    `json:"region"`
	Capacity  int       `json:"capacity_conns"`
	Version   string    `json:"version"`
	State     string    `json:"state"` // healthy | degraded | draining | ejected | offline
	Draining  bool      `json:"draining"`
	LastSeen  time.Time `json:"last_seen"`
	JoinedAt  time.Time `json:"joined_at"`

	Score       float64 `json:"score"` // current routing score (lower = better)
	CPUPercent  float64 `json:"cpu_percent"`
	RAMPercent  float64 `json:"ram_percent"`
	DiskPercent float64 `json:"disk_percent"`
	ActiveConns int     `json:"active_conns"`

	BytesOutRate float64 `json:"bytes_out_rate"` // bytes/sec, derived
	BytesInRate  float64 `json:"bytes_in_rate"`
	HitRatio     float64 `json:"hit_ratio"` // 0..1 lifetime
	CacheBytes   uint64  `json:"cache_bytes"`
	CacheFiles   int     `json:"cache_files"`

	UptimeSeconds int64 `json:"uptime_seconds"`
	RoutedTotal   int64 `json:"routed_total"` // playback sessions routed here
}

// TelemetrySample is one point of a node's stored history, used by the
// console charts.
type TelemetrySample struct {
	T            time.Time `json:"t"`
	CPU          float64   `json:"cpu"`
	RAM          float64   `json:"ram"`
	Disk         float64   `json:"disk"`
	Conns        int       `json:"conns"`
	BytesOutRate float64   `json:"out_rate"`
	BytesInRate  float64   `json:"in_rate"`
	HitRatio     float64   `json:"hit"`
}

// ClusterStats is the aggregate overview for the console and `hpcdn status`.
type ClusterStats struct {
	Nodes         int     `json:"nodes"`
	EdgesHealthy  int     `json:"edges_healthy"`
	EdgesTotal    int     `json:"edges_total"`
	OriginsTotal  int     `json:"origins_total"`
	RoutedTotal   int64   `json:"routed_total"`
	RoutedPerSec  float64 `json:"routed_per_sec"`
	BytesOutRate  float64 `json:"bytes_out_rate"`
	HitRatio      float64 `json:"hit_ratio"`
	CacheBytes    uint64  `json:"cache_bytes"`
	UptimeSeconds int64   `json:"uptime_seconds"`
}

// SignRequest asks the controller to mint a signed playback URL.
type SignRequest struct {
	Path       string `json:"path"`                  // e.g. /play/stream1/index.m3u8
	Scope      string `json:"scope,omitempty"`       // optional prefix scope
	TTLSeconds int    `json:"ttl_seconds,omitempty"` // default from settings
}

// SignResponse is the minted URL.
type SignResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PurgeRequest broadcasts a purge to all edges.
type PurgeRequest struct {
	Path string `json:"path"` // path prefix, e.g. /stream1/
}

// JoinTokenInfo describes an enrollment token (admin API).
type JoinTokenInfo struct {
	ID        string     `json:"id"`
	Token     string     `json:"token,omitempty"` // only returned at creation
	Note      string     `json:"note,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	MaxUses   int        `json:"max_uses,omitempty"` // 0 = unlimited
	Uses      int        `json:"uses"`
}

// Settings are the live-tunable routing parameters exposed on the admin
// API and the console settings page.
type Settings struct {
	CPUWeight       float64 `json:"cpu_weight"`
	RAMWeight       float64 `json:"ram_weight"`
	ConnWeight      float64 `json:"conn_weight"`
	RegionPenalty   float64 `json:"region_penalty"`   // score penalty for cross-region routing
	SaturationScore float64 `json:"saturation_score"` // bounded-load threshold (0-100)
	EWMAAlpha       float64 `json:"ewma_alpha"`       // telemetry smoothing factor 0..1
	HeartbeatTTL    int     `json:"heartbeat_ttl_seconds"`
	EjectAfterMiss  int     `json:"eject_after_missed"` // heartbeats missed before ejection
	PanicThreshold  float64 `json:"panic_threshold"`    // fraction of pool ejected before panic routing
	SignTTLSeconds  int     `json:"sign_ttl_seconds"`   // default signed URL lifetime
	AffinityEnabled bool    `json:"affinity_enabled"`   // rendezvous cache-affinity routing
}

// DefaultSettings returns production-reasonable defaults.
func DefaultSettings() Settings {
	return Settings{
		CPUWeight:       0.5,
		RAMWeight:       0.2,
		ConnWeight:      0.3,
		RegionPenalty:   30,
		SaturationScore: 75,
		EWMAAlpha:       0.3,
		HeartbeatTTL:    10,
		EjectAfterMiss:  3,
		PanicThreshold:  0.5,
		SignTTLSeconds:  6 * 3600,
		AffinityEnabled: true,
	}
}

// Event is pushed on the controller's SSE stream to live-update consoles.
type Event struct {
	Time time.Time `json:"time"`
	Type string    `json:"type"` // node_joined | node_offline | node_ejected | purge | settings | route
	Node string    `json:"node,omitempty"`
	Msg  string    `json:"msg"`
}
