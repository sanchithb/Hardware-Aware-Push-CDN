// Package config loads hpcdn component configuration with a clear
// precedence chain: built-in defaults < YAML file < environment variables
// (HPCDN_*) < command-line flags. Every field is documented in
// configs/*.yaml examples.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Common holds fields shared by every component.
type Common struct {
	// Listen is the host:port the component's HTTP server binds.
	Listen string `yaml:"listen"`
	// PublicURL is how other nodes / clients reach this component. If
	// empty it is derived from Listen (useful only on one machine).
	PublicURL string `yaml:"public_url"`
	// DataDir is where the component persists its state.
	DataDir string `yaml:"data_dir"`
	// LogLevel: debug | info | warn | error.
	LogLevel string `yaml:"log_level"`
	// LogFormat: text | json.
	LogFormat string `yaml:"log_format"`
	// TLS enables HTTPS with either provided cert/key or an in-memory
	// self-signed certificate when both paths are empty.
	TLS struct {
		Enabled  bool   `yaml:"enabled"`
		CertFile string `yaml:"cert_file"`
		KeyFile  string `yaml:"key_file"`
	} `yaml:"tls"`
}

// Controller is the control-plane configuration.
type Controller struct {
	Common `yaml:",inline"`
	// AdminKey overrides the generated admin API key (useful in IaC).
	AdminKey string `yaml:"admin_key"`
	// RoutingMode: redirect (302 to edge) | proxy (stream through controller).
	RoutingMode string `yaml:"routing_mode"`
	// TelemetryHistory is how many heartbeat samples are kept per node
	// for console charts.
	TelemetryHistory int `yaml:"telemetry_history"`
	// HeartbeatInterval instructs nodes how often to report, seconds.
	HeartbeatInterval int `yaml:"heartbeat_interval"`
}

// Node holds fields shared by edge and origin data-plane components.
type Node struct {
	Common `yaml:",inline"`
	// ControllerURL is the control-plane base URL.
	ControllerURL string `yaml:"controller_url"`
	// JoinToken enrolls this node on first start. After enrollment the
	// issued identity is persisted in DataDir and the token is unused.
	JoinToken string `yaml:"join_token"`
	// Name is a human-readable node name (defaults to hostname).
	Name string `yaml:"name"`
	// Region is the point-of-presence label used for geo routing.
	Region string `yaml:"region"`
	// InsecureSkipVerify disables TLS verification for intra-cluster
	// calls (self-signed certificates in lab setups).
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

// Edge configures an edge node.
type Edge struct {
	Node `yaml:",inline"`
	// CacheDir is where content is stored. Defaults under DataDir.
	CacheDir string `yaml:"cache_dir"`
	// CacheMaxBytes caps cache disk usage; SIEVE eviction keeps below it.
	CacheMaxBytes int64 `yaml:"cache_max_bytes"`
	// Capacity is the soft concurrent-connection capacity reported to the
	// controller for load scoring.
	Capacity int `yaml:"capacity_conns"`
	// HoldTimeout is how long /play waits for a not-yet-pushed segment
	// before falling back to origin pull (live-edge hold-and-serve).
	HoldTimeout time.Duration `yaml:"hold_timeout"`
	// PlaylistTTL / SegmentTTL control Cache-Control emitted to players.
	PlaylistTTL time.Duration `yaml:"playlist_ttl"`
	SegmentTTL  time.Duration `yaml:"segment_ttl"`
}

// Origin configures an origin node.
type Origin struct {
	Node `yaml:",inline"`
	// WatchDir is the content root that is watched and pushed.
	WatchDir string `yaml:"watch_dir"`
	// PushWorkers is the size of the concurrent push worker pool.
	PushWorkers int `yaml:"push_workers"`
	// PushRetries is per-file per-edge delivery attempts.
	PushRetries int `yaml:"push_retries"`
	// DebounceWindow coalesces rapid fsnotify events per file.
	DebounceWindow time.Duration `yaml:"debounce_window"`
	// PushExtensions limits which file types are pushed eagerly. Others
	// remain available via pull-through. Empty = push everything.
	PushExtensions []string `yaml:"push_extensions"`
}

func defaultDataDir(component string) string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = "."
	}
	return filepath.Join(base, "hpcdn", component)
}

// DefaultController returns controller defaults.
func DefaultController() Controller {
	c := Controller{}
	c.Listen = ":8080"
	c.DataDir = defaultDataDir("controller")
	c.LogLevel = "info"
	c.LogFormat = "text"
	c.RoutingMode = "redirect"
	c.TelemetryHistory = 360
	c.HeartbeatInterval = 2
	return c
}

// DefaultEdge returns edge defaults.
func DefaultEdge() Edge {
	e := Edge{}
	e.Listen = ":8081"
	e.DataDir = defaultDataDir("edge")
	e.LogLevel = "info"
	e.LogFormat = "text"
	e.ControllerURL = "http://127.0.0.1:8080"
	e.CacheMaxBytes = 10 << 30 // 10 GiB
	e.Capacity = 1000
	e.HoldTimeout = 3 * time.Second
	e.PlaylistTTL = 2 * time.Second
	e.SegmentTTL = 365 * 24 * time.Hour
	return e
}

// DefaultOrigin returns origin defaults.
func DefaultOrigin() Origin {
	o := Origin{}
	o.Listen = ":9000"
	o.DataDir = defaultDataDir("origin")
	o.LogLevel = "info"
	o.LogFormat = "text"
	o.ControllerURL = "http://127.0.0.1:8080"
	o.WatchDir = "./content"
	o.PushWorkers = 8
	o.PushRetries = 3
	o.DebounceWindow = 200 * time.Millisecond
	o.PushExtensions = []string{".ts", ".m3u8", ".mp4", ".m4s", ".mpd", ".vtt"}
	return o
}

// LoadFile merges a YAML file over cfg (a pointer to one of the config
// structs). A missing file is only an error when explicit is true.
func LoadFile(path string, cfg any, explicit bool) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return nil
		}
		return fmt.Errorf("config: reading %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return fmt.Errorf("config: parsing %s: %w", path, err)
	}
	return nil
}

// envString overrides dst when HPCDN_<name> is set.
func envString(name string, dst *string) {
	if v, ok := os.LookupEnv("HPCDN_" + name); ok {
		*dst = v
	}
}

func envBool(name string, dst *bool) {
	if v, ok := os.LookupEnv("HPCDN_" + name); ok {
		*dst = v == "1" || strings.EqualFold(v, "true")
	}
}

func envInt64(name string, dst *int64) {
	if v, ok := os.LookupEnv("HPCDN_" + name); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			*dst = n
		}
	}
}

// ApplyCommonEnv applies HPCDN_* environment overrides for shared fields.
func ApplyCommonEnv(c *Common) {
	envString("LISTEN", &c.Listen)
	envString("PUBLIC_URL", &c.PublicURL)
	envString("DATA_DIR", &c.DataDir)
	envString("LOG_LEVEL", &c.LogLevel)
	envString("LOG_FORMAT", &c.LogFormat)
	envBool("TLS", &c.TLS.Enabled)
	envString("TLS_CERT", &c.TLS.CertFile)
	envString("TLS_KEY", &c.TLS.KeyFile)
}

// ApplyNodeEnv applies environment overrides for node fields.
func ApplyNodeEnv(n *Node) {
	ApplyCommonEnv(&n.Common)
	envString("CONTROLLER_URL", &n.ControllerURL)
	envString("JOIN_TOKEN", &n.JoinToken)
	envString("NAME", &n.Name)
	envString("REGION", &n.Region)
	envBool("INSECURE_SKIP_VERIFY", &n.InsecureSkipVerify)
}

// ApplyControllerEnv applies environment overrides for the controller.
func ApplyControllerEnv(c *Controller) {
	ApplyCommonEnv(&c.Common)
	envString("ADMIN_KEY", &c.AdminKey)
	envString("ROUTING_MODE", &c.RoutingMode)
}

// ApplyEdgeEnv applies environment overrides for an edge.
func ApplyEdgeEnv(e *Edge) {
	ApplyNodeEnv(&e.Node)
	envString("CACHE_DIR", &e.CacheDir)
	envInt64("CACHE_MAX_BYTES", &e.CacheMaxBytes)
}

// ApplyOriginEnv applies environment overrides for an origin.
func ApplyOriginEnv(o *Origin) {
	ApplyNodeEnv(&o.Node)
	envString("WATCH_DIR", &o.WatchDir)
}
