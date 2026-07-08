// Package metrics is a dependency-free Prometheus exposition-format
// registry. It implements the small subset hpcdn needs — counters, gauges
// and cumulative histograms with constant labels — and serves them at
// /metrics in the standard text format so any Prometheus-compatible
// scraper (Prometheus, VictoriaMetrics, Grafana Agent, Datadog) works
// out of the box.
package metrics

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds named metric families.
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*Counter
	gauges   map[string]*Gauge
	hists    map[string]*Histogram
	help     map[string]string
	types    map[string]string
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		counters: map[string]*Counter{},
		gauges:   map[string]*Gauge{},
		hists:    map[string]*Histogram{},
		help:     map[string]string{},
		types:    map[string]string{},
	}
}

// key renders name plus sorted label pairs into a stable series key.
func key(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	ks := make([]string, 0, len(labels))
	for k := range labels {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var sb strings.Builder
	sb.WriteString(name)
	sb.WriteByte('{')
	for i, k := range ks {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%s=%q", k, labels[k])
	}
	sb.WriteByte('}')
	return sb.String()
}

// Counter is a monotonically increasing float64.
type Counter struct{ bits atomic.Uint64 }

// Add increments the counter by v (v must be >= 0).
func (c *Counter) Add(v float64) {
	for {
		old := c.bits.Load()
		nw := math.Float64bits(math.Float64frombits(old) + v)
		if c.bits.CompareAndSwap(old, nw) {
			return
		}
	}
}

// Inc adds 1.
func (c *Counter) Inc() { c.Add(1) }

// Value returns the current value.
func (c *Counter) Value() float64 { return math.Float64frombits(c.bits.Load()) }

// Gauge is an arbitrary float64.
type Gauge struct{ bits atomic.Uint64 }

// Set replaces the gauge value.
func (g *Gauge) Set(v float64) { g.bits.Store(math.Float64bits(v)) }

// Add increments the gauge by v (may be negative).
func (g *Gauge) Add(v float64) {
	for {
		old := g.bits.Load()
		nw := math.Float64bits(math.Float64frombits(old) + v)
		if g.bits.CompareAndSwap(old, nw) {
			return
		}
	}
}

// Value returns the current value.
func (g *Gauge) Value() float64 { return math.Float64frombits(g.bits.Load()) }

// Histogram is a cumulative histogram with fixed upper bounds.
type Histogram struct {
	mu     sync.Mutex
	bounds []float64
	counts []uint64
	sum    float64
	total  uint64
}

// Observe records one sample.
func (h *Histogram) Observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i]++
		}
	}
	h.sum += v
	h.total++
}

// DefBuckets are latency buckets in seconds suited to routing/serving paths.
var DefBuckets = []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// Counter registers (or fetches) a counter series.
func (r *Registry) Counter(name, help string, labels map[string]string) *Counter {
	k := key(name, labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[k]; ok {
		return c
	}
	c := &Counter{}
	r.counters[k] = c
	r.help[name] = help
	r.types[name] = "counter"
	return c
}

// Gauge registers (or fetches) a gauge series.
func (r *Registry) Gauge(name, help string, labels map[string]string) *Gauge {
	k := key(name, labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[k]; ok {
		return g
	}
	g := &Gauge{}
	r.gauges[k] = g
	r.help[name] = help
	r.types[name] = "gauge"
	return g
}

// Histogram registers (or fetches) a histogram series with DefBuckets.
func (r *Registry) Histogram(name, help string, labels map[string]string) *Histogram {
	k := key(name, labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.hists[k]; ok {
		return h
	}
	h := &Histogram{bounds: DefBuckets, counts: make([]uint64, len(DefBuckets))}
	r.hists[k] = h
	r.help[name] = help
	r.types[name] = "histogram"
	return h
}

func splitSeries(k string) (name, labels string) {
	if i := strings.IndexByte(k, '{'); i >= 0 {
		return k[:i], k[i:]
	}
	return k, ""
}

// Render writes the registry in Prometheus text exposition format.
func (r *Registry) Render(w *strings.Builder) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	emitted := map[string]bool{}
	header := func(name string) {
		if !emitted[name] {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, r.help[name], name, r.types[name])
			emitted[name] = true
		}
	}

	keys := make([]string, 0, len(r.counters))
	for k := range r.counters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name, lbl := splitSeries(k)
		header(name)
		fmt.Fprintf(w, "%s%s %g\n", name, lbl, r.counters[k].Value())
	}

	keys = keys[:0]
	for k := range r.gauges {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name, lbl := splitSeries(k)
		header(name)
		fmt.Fprintf(w, "%s%s %g\n", name, lbl, r.gauges[k].Value())
	}

	keys = keys[:0]
	for k := range r.hists {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name, lbl := splitSeries(k)
		header(name)
		h := r.hists[k]
		h.mu.Lock()
		base := strings.TrimSuffix(lbl, "}")
		for i, b := range h.bounds {
			sep := "{"
			if base != "" {
				sep = base + ","
			}
			fmt.Fprintf(w, "%s_bucket%sle=\"%g\"} %d\n", name, sep, b, h.counts[i])
		}
		sep := "{"
		if base != "" {
			sep = base + ","
		}
		fmt.Fprintf(w, "%s_bucket%sle=\"+Inf\"} %d\n", name, sep, h.total)
		fmt.Fprintf(w, "%s_sum%s %g\n", name, lbl, h.sum)
		fmt.Fprintf(w, "%s_count%s %d\n", name, lbl, h.total)
		h.mu.Unlock()
	}
}

// Handler returns an http.Handler serving the exposition format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var sb strings.Builder
		r.Render(&sb)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(sb.String()))
	})
}
