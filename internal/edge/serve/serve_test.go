package serve

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/edge/cache"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/auth"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/metrics"
)

const (
	signKey   = "hps_sign"
	ingestKey = "hpi_ingest"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	c, err := cache.Open(t.TempDir(), 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Cache:         c,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:       metrics.NewRegistry(),
		SigningKey:    signKey,
		IngestKey:     ingestKey,
		ControllerURL: "http://ctrl.example",
		Rewrite:       RewriteController,
		HoldTimeout:   300 * time.Millisecond,
		PlaylistTTL:   2 * time.Second,
		SegmentTTL:    time.Hour,
	}
	mux := http.NewServeMux()
	s.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return s, ts
}

func ingest(t *testing.T, ts *httptest.Server, key, body string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/ingest/"+key, strings.NewReader(body))
	req.Header.Set(protocol.HeaderIngestKey, ingestKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("ingest returned %d", resp.StatusCode)
	}
}

func signedURL(ts *httptest.Server, path string) string {
	return ts.URL + path + "?" + auth.SignPath(signKey, path, "/play/stream1/", time.Hour)
}

func TestIngestRequiresKey(t *testing.T) {
	_, ts := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/ingest/a.ts", strings.NewReader("x"))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated ingest accepted: %d", resp.StatusCode)
	}
}

func TestPlayRequiresSignature(t *testing.T) {
	_, ts := newTestServer(t)
	ingest(t, ts, "stream1/seg0.ts", "data")
	resp, _ := http.Get(ts.URL + "/play/stream1/seg0.ts")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned playback allowed: %d", resp.StatusCode)
	}
}

func TestSegmentHeaders(t *testing.T) {
	_, ts := newTestServer(t)
	ingest(t, ts, "stream1/seg0.ts", "segment-bytes")
	resp, err := http.Get(signedURL(ts, "/play/stream1/seg0.ts"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	cc := resp.Header.Get("Cache-Control")
	if !strings.Contains(cc, "immutable") {
		t.Errorf("segments must be immutable, got %q", cc)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("CORS header missing")
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Error("range support missing")
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "segment-bytes" {
		t.Errorf("body mismatch: %q", b)
	}
}

func TestPlaylistRewriteToController(t *testing.T) {
	_, ts := newTestServer(t)
	m3u8 := "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:2.0,\nseg0.ts\n#EXTINF:2.0,\nsub/seg1.ts\n"
	ingest(t, ts, "stream1/index.m3u8", m3u8)

	resp, err := http.Get(signedURL(ts, "/play/stream1/index.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	cc := resp.Header.Get("Cache-Control")
	if strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=2") {
		t.Errorf("playlist cache-control wrong: %q", cc)
	}
	if !strings.Contains(out, "http://ctrl.example/play/stream1/seg0.ts?") {
		t.Errorf("segment URI not rewritten through controller:\n%s", out)
	}
	if !strings.Contains(out, `URI="http://ctrl.example/play/stream1/init.mp4?`) {
		t.Errorf("EXT-X-MAP URI not rewritten:\n%s", out)
	}
	// The rewritten URIs must carry a signature valid for their own path.
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "http://") {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("bad rewritten URI %q: %v", line, err)
		}
		if err := auth.ValidateQuery(signKey, u.Path, u.Query(), time.Now()); err != nil {
			t.Errorf("rewritten URI %s has invalid signature: %v", u.Path, err)
		}
	}
}

func TestHoldAndServe(t *testing.T) {
	s, ts := newTestServer(t)
	s.HoldTimeout = 2 * time.Second

	// Request a segment that arrives 150ms later — the classic live-edge race.
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Get(signedURL(ts, "/play/stream1/late.ts"))
		if err == nil {
			done <- resp
		}
	}()
	time.Sleep(150 * time.Millisecond)
	ingest(t, ts, "stream1/late.ts", "arrived-late")

	select {
	case resp := <-done:
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("hold-and-serve returned %d", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		if string(b) != "arrived-late" {
			t.Fatalf("wrong body: %q", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hold-and-serve timed out")
	}
}

func TestMissWithNoOrigins404s(t *testing.T) {
	s, ts := newTestServer(t)
	s.HoldTimeout = 100 * time.Millisecond
	resp, _ := http.Get(signedURL(ts, "/play/stream1/never.ts"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown object, got %d", resp.StatusCode)
	}
}

func TestPullThroughFromOrigin(t *testing.T) {
	s, ts := newTestServer(t)

	originHits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(protocol.HeaderIngestKey) != ingestKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		originHits++
		_, _ = w.Write([]byte("from-origin"))
	}))
	defer origin.Close()
	s.SetOrigins([]protocol.OriginEndpoint{{NodeID: "o1", FetchURL: origin.URL + "/fetch"}})

	resp, err := http.Get(signedURL(ts, "/play/stream1/cold.ts"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(b) != "from-origin" {
		t.Fatalf("pull-through failed: %d %q", resp.StatusCode, b)
	}
	// Second request must be a cache hit, not another origin fetch.
	resp2, _ := http.Get(signedURL(ts, "/play/stream1/cold.ts"))
	resp2.Body.Close()
	if originHits != 1 {
		t.Fatalf("expected 1 origin fetch, got %d", originHits)
	}
}

func TestDrainRefusesNewSessions(t *testing.T) {
	s, ts := newTestServer(t)
	ingest(t, ts, "stream1/seg0.ts", "x")
	s.SetDraining(true)
	resp, _ := http.Get(signedURL(ts, "/play/stream1/seg0.ts"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("draining edge served new session: %d", resp.StatusCode)
	}
}
