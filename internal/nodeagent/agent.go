// Package nodeagent implements the client side of the control-plane
// protocol shared by edge and origin nodes: one-time enrollment with a
// join token, durable identity storage, and the periodic heartbeat loop
// that reports hardware telemetry and receives directives + topology.
package nodeagent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/version"
)

// Identity is the persisted result of enrollment.
type Identity struct {
	NodeID     string `json:"node_id"`
	NodeSecret string `json:"node_secret"`
	SigningKey string `json:"signing_key"`
	IngestKey  string `json:"ingest_key"`
}

// Stats are the node-specific counters merged into each heartbeat.
// Implementations must be safe for concurrent use.
type Stats interface {
	Snapshot() (activeConns int, bytesOut, bytesIn, cacheHits, cacheMisses uint64, cacheBytes uint64, cacheFiles int)
}

// Agent runs enrollment and the heartbeat loop for one node process.
type Agent struct {
	ControllerURL string
	Kind          protocol.NodeKind
	Name          string
	PublicURL     string
	Region        string
	Capacity      int
	DataDir       string
	CacheDir      string // disk telemetry is measured on this volume; may be ""
	Log           *slog.Logger
	Stats         Stats

	// OnDirective is called for each directive received (at-least-once;
	// implementations should be idempotent).
	OnDirective func(protocol.Directive)
	// OnTopology receives the peer lists from each heartbeat response.
	OnTopology func(edges []protocol.EdgeEndpoint, origins []protocol.OriginEndpoint)

	HTTP     *http.Client
	identity Identity
	interval time.Duration
	ackSeq   atomic.Uint64
	started  time.Time
}

// NewHTTPClient builds the intra-cluster HTTP client.
func NewHTTPClient(insecureSkipVerify bool, timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // lab mode: self-signed intra-cluster TLS
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

// Identity returns the node identity (valid after Enroll).
func (a *Agent) Identity() Identity { return a.identity }

func (a *Agent) identityPath() string {
	return filepath.Join(a.DataDir, "identity.json")
}

// Enroll loads a persisted identity, or registers with joinToken when the
// node is new. It retries registration until ctx is cancelled, so nodes
// can start before the controller.
func (a *Agent) Enroll(ctx context.Context, joinToken string) error {
	if a.HTTP == nil {
		a.HTTP = NewHTTPClient(false, 10*time.Second)
	}
	a.started = time.Now()
	if err := os.MkdirAll(a.DataDir, 0o700); err != nil {
		return fmt.Errorf("nodeagent: creating data dir: %w", err)
	}

	if b, err := os.ReadFile(a.identityPath()); err == nil {
		if err := json.Unmarshal(b, &a.identity); err == nil && a.identity.NodeID != "" {
			a.Log.Info("loaded node identity", "node_id", a.identity.NodeID)
			return nil
		}
		a.Log.Warn("identity file unreadable, re-enrolling")
	}

	if joinToken == "" {
		return fmt.Errorf("nodeagent: node is not enrolled and no join token was provided (set join_token or HPCDN_JOIN_TOKEN)")
	}

	req := protocol.RegisterRequest{
		JoinToken: joinToken,
		Kind:      a.Kind,
		Name:      a.Name,
		PublicURL: a.PublicURL,
		Region:    a.Region,
		Capacity:  a.Capacity,
		Version:   version.Version,
	}
	body, _ := json.Marshal(req)

	backoff := time.Second
	for {
		resp, err := a.post(ctx, a.ControllerURL+"/api/v1/nodes/register", body, nil)
		if err == nil {
			var rr protocol.RegisterResponse
			derr := json.NewDecoder(resp.Body).Decode(&rr)
			code := resp.StatusCode
			resp.Body.Close()
			switch {
			case code == http.StatusCreated && derr == nil:
				a.identity = Identity{
					NodeID: rr.NodeID, NodeSecret: rr.NodeSecret,
					SigningKey: rr.SigningKey, IngestKey: rr.IngestKey,
				}
				a.interval = time.Duration(rr.HeartbeatInterval) * time.Second
				b, _ := json.MarshalIndent(a.identity, "", "  ")
				if werr := os.WriteFile(a.identityPath(), b, 0o600); werr != nil {
					return fmt.Errorf("nodeagent: persisting identity: %w", werr)
				}
				a.Log.Info("enrolled with controller", "node_id", rr.NodeID)
				return nil
			case code == http.StatusUnauthorized:
				return fmt.Errorf("nodeagent: controller rejected join token")
			default:
				a.Log.Warn("registration attempt failed", "status", code, "err", derr)
			}
		} else {
			a.Log.Warn("controller unreachable, retrying", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			if backoff < 15*time.Second {
				backoff *= 2
			}
		}
	}
}

func (a *Agent) post(ctx context.Context, url string, body []byte, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return a.HTTP.Do(req)
}

// RunHeartbeats blocks, reporting telemetry until ctx is cancelled.
func (a *Agent) RunHeartbeats(ctx context.Context) {
	if a.interval <= 0 {
		a.interval = 2 * time.Second
	}
	// Prime the CPU sampler so the first real reading is meaningful.
	_, _ = cpu.PercentWithContext(ctx, 0, false)

	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.beat(ctx)
		}
	}
}

func (a *Agent) beat(ctx context.Context) {
	hb := protocol.Heartbeat{
		Goroutines:    runtime.NumGoroutine(),
		UptimeSeconds: int64(time.Since(a.started).Seconds()),
		Version:       version.Version,
		AckSeq:        a.ackSeq.Load(),
	}
	if pct, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(pct) > 0 {
		hb.CPUPercent = pct[0]
	}
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		hb.RAMPercent = vm.UsedPercent
	}
	if a.CacheDir != "" {
		if du, err := disk.UsageWithContext(ctx, a.CacheDir); err == nil {
			hb.DiskPercent = du.UsedPercent
		}
	}
	if a.Stats != nil {
		conns, out, in, hits, misses, cbytes, cfiles := a.Stats.Snapshot()
		hb.ActiveConns = conns
		hb.BytesOut, hb.BytesIn = out, in
		hb.CacheHits, hb.CacheMisses = hits, misses
		hb.CacheBytes, hb.CacheFiles = cbytes, cfiles
	}

	body, _ := json.Marshal(hb)
	url := fmt.Sprintf("%s/api/v1/nodes/%s/heartbeat", a.ControllerURL, a.identity.NodeID)
	resp, err := a.post(ctx, url, body, map[string]string{
		protocol.HeaderNodeSecret: a.identity.NodeSecret,
	})
	if err != nil {
		a.Log.Debug("heartbeat failed", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		a.Log.Warn("heartbeat rejected", "status", resp.StatusCode)
		return
	}
	var hr protocol.HeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		return
	}
	for _, d := range hr.Directives {
		if a.OnDirective != nil {
			a.OnDirective(d)
		}
		if d.Seq > a.ackSeq.Load() {
			a.ackSeq.Store(d.Seq)
		}
	}
	if a.OnTopology != nil {
		a.OnTopology(hr.Edges, hr.Origins)
	}
}
