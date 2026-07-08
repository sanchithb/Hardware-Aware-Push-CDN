package metrics

import (
	"strings"
	"testing"
)

func TestCounterGaugeRender(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("requests_total", "Total requests", map[string]string{"code": "200"})
	c.Add(3)
	c.Inc()
	g := r.Gauge("queue_depth", "Depth", nil)
	g.Set(7)
	g.Add(-2)

	var sb strings.Builder
	r.Render(&sb)
	out := sb.String()

	for _, want := range []string{
		"# HELP requests_total Total requests",
		"# TYPE requests_total counter",
		`requests_total{code="200"} 4`,
		"# TYPE queue_depth gauge",
		"queue_depth 5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestHistogramCumulativeBuckets(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("latency_seconds", "Latency", nil)
	h.Observe(0.0004) // below first bound
	h.Observe(0.003)
	h.Observe(9)

	var sb strings.Builder
	r.Render(&sb)
	out := sb.String()

	for _, want := range []string{
		`latency_seconds_bucket{le="0.0005"} 1`,
		`latency_seconds_bucket{le="0.005"} 2`,
		`latency_seconds_bucket{le="10"} 3`,
		`latency_seconds_bucket{le="+Inf"} 3`,
		"latency_seconds_count 3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSameSeriesReturned(t *testing.T) {
	r := NewRegistry()
	a := r.Counter("x_total", "x", map[string]string{"a": "1"})
	b := r.Counter("x_total", "x", map[string]string{"a": "1"})
	if a != b {
		t.Fatal("same labels must return the same series")
	}
	c := r.Counter("x_total", "x", map[string]string{"a": "2"})
	if a == c {
		t.Fatal("different labels must be distinct series")
	}
}
