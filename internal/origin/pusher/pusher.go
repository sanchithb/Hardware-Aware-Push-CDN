// Package pusher delivers content to edge nodes: a bounded worker pool
// pushes each file to every registered edge concurrently, with SHA-256
// integrity headers, exponential-backoff retries, and per-edge failure
// accounting. Files an edge misses (or that predate an edge joining) stay
// reachable through the origin's /fetch pull-through endpoint.
package pusher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/metrics"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/version"
)

// Job is one file to distribute.
type Job struct {
	AbsPath string
	RelPath string // forward-slash key under the content root
}

// Pusher runs the distribution pipeline.
type Pusher struct {
	Root      string
	IngestKey string
	Workers   int
	Retries   int
	Log       *slog.Logger
	HTTP      *http.Client
	Metrics   *metrics.Registry

	edges atomic.Value // []protocol.EdgeEndpoint

	jobs      chan Job
	pushed    *metrics.Counter
	failed    *metrics.Counter
	bytesSent *metrics.Counter
	queueLen  *metrics.Gauge

	bytesOut atomic.Uint64
}

// SetEdges updates the target edge set (from heartbeat topology).
func (p *Pusher) SetEdges(edges []protocol.EdgeEndpoint) { p.edges.Store(edges) }

// Edges returns the current target set.
func (p *Pusher) Edges() []protocol.EdgeEndpoint {
	e, _ := p.edges.Load().([]protocol.EdgeEndpoint)
	return e
}

// BytesOut returns cumulative bytes pushed (for heartbeats).
func (p *Pusher) BytesOut() uint64 { return p.bytesOut.Load() }

// Start launches the worker pool. Enqueue jobs with Push.
func (p *Pusher) Start(ctx context.Context) {
	if p.Workers <= 0 {
		p.Workers = 8
	}
	if p.Retries <= 0 {
		p.Retries = 3
	}
	if p.HTTP == nil {
		p.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	p.jobs = make(chan Job, 4096)
	p.pushed = p.Metrics.Counter("hpcdn_origin_pushes_total", "Successful segment deliveries to edges", nil)
	p.failed = p.Metrics.Counter("hpcdn_origin_push_failures_total", "Deliveries that exhausted retries", nil)
	p.bytesSent = p.Metrics.Counter("hpcdn_origin_push_bytes_total", "Bytes delivered to edges", nil)
	p.queueLen = p.Metrics.Gauge("hpcdn_origin_queue_depth", "Pending distribution jobs", nil)

	for i := 0; i < p.Workers; i++ {
		go p.worker(ctx)
	}
}

// Push enqueues a file for distribution. Returns false if the queue is full.
func (p *Pusher) Push(j Job) bool {
	select {
	case p.jobs <- j:
		p.queueLen.Set(float64(len(p.jobs)))
		return true
	default:
		p.Log.Error("distribution queue full, dropping job", "path", j.RelPath)
		return false
	}
}

func (p *Pusher) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-p.jobs:
			p.queueLen.Set(float64(len(p.jobs)))
			p.distribute(ctx, j)
		}
	}
}

// distribute pushes one file to every edge concurrently.
func (p *Pusher) distribute(ctx context.Context, j Job) {
	content, err := os.ReadFile(j.AbsPath)
	if err != nil {
		p.Log.Warn("cannot read file for push", "path", j.AbsPath, "err", err)
		return
	}
	sum := sha256.Sum256(content)
	checksum := hex.EncodeToString(sum[:])

	edges := p.Edges()
	if len(edges) == 0 {
		p.Log.Debug("no edges registered; content stays pull-through only", "path", j.RelPath)
		return
	}

	var wg sync.WaitGroup
	for _, e := range edges {
		wg.Add(1)
		go func(e protocol.EdgeEndpoint) {
			defer wg.Done()
			if err := p.pushOne(ctx, e, j.RelPath, content, checksum); err != nil {
				p.failed.Inc()
				p.Log.Warn("push failed after retries", "edge", e.NodeID, "path", j.RelPath, "err", err)
				return
			}
			p.pushed.Inc()
			p.bytesSent.Add(float64(len(content)))
			p.bytesOut.Add(uint64(len(content)))
			p.Log.Info("pushed", "path", j.RelPath, "edge", e.NodeID, "bytes", len(content))
		}(e)
	}
	wg.Wait()
}

func (p *Pusher) pushOne(ctx context.Context, e protocol.EdgeEndpoint, rel string, content []byte, checksum string) error {
	url := strings.TrimRight(e.IngestURL, "/") + "/" + rel
	backoff := 250 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < p.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
				backoff *= 2
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(content))
		if err != nil {
			return err
		}
		req.Header.Set(protocol.HeaderIngestKey, p.IngestKey)
		req.Header.Set(protocol.HeaderChecksum, checksum)
		req.Header.Set("User-Agent", version.UserAgent())
		resp, err := p.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
			return nil
		}
		lastErr = fmt.Errorf("edge returned %d", resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusUnprocessableEntity {
			return lastErr // retrying won't change an auth/integrity failure
		}
	}
	return lastErr
}

// RelKey converts an absolute path under root to a cache key.
func RelKey(root, abs string) (string, error) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}
