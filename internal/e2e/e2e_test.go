// Package e2e boots a complete cluster in-process — controller, origin,
// edge — and exercises the full content path: enrollment via join token,
// watch→push distribution, signed playback through the routing layer,
// playlist rewriting, purge and pull-through recovery.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/edge"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/origin"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/config"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/logx"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("service at %s never became healthy", url)
}

type adminAPI struct {
	base string
	key  string
}

func (a adminAPI) call(t *testing.T, method, path string, body any, out any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+a.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s -> %d: %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
}

func TestFullClusterLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e test skipped in -short mode")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctlPort, edgePort, originPort := freePort(t), freePort(t), freePort(t)
	ctlURL := fmt.Sprintf("http://127.0.0.1:%d", ctlPort)

	// --- controller ---
	ccfg := config.DefaultController()
	ccfg.Listen = fmt.Sprintf("127.0.0.1:%d", ctlPort)
	ccfg.DataDir = filepath.Join(t.TempDir(), "ctl")
	ccfg.AdminKey = "hpa_e2e-test-key"
	ctl, err := controller.New(ccfg, quiet, logx.NewRing(64))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = ctl.Run(ctx) }()
	waitHTTP(t, ctlURL+"/healthz", 10*time.Second)
	api := adminAPI{base: ctlURL, key: "hpa_e2e-test-key"}

	// --- join token ---
	var tok protocol.JoinTokenInfo
	api.call(t, http.MethodPost, "/api/v1/tokens", map[string]any{"note": "e2e"}, &tok)
	if tok.Token == "" {
		t.Fatal("no token issued")
	}

	// --- origin ---
	watchDir := filepath.Join(t.TempDir(), "content")
	ocfg := config.DefaultOrigin()
	ocfg.Listen = fmt.Sprintf("127.0.0.1:%d", originPort)
	ocfg.PublicURL = fmt.Sprintf("http://127.0.0.1:%d", originPort)
	ocfg.DataDir = filepath.Join(t.TempDir(), "origin")
	ocfg.ControllerURL = ctlURL
	ocfg.JoinToken = tok.Token
	ocfg.WatchDir = watchDir
	ocfg.DebounceWindow = 100 * time.Millisecond
	og := &origin.Origin{Cfg: ocfg, Log: quiet}
	go func() { _ = og.Run(ctx) }()
	waitHTTP(t, ocfg.PublicURL+"/healthz", 10*time.Second)

	// --- edge ---
	ecfg := config.DefaultEdge()
	ecfg.Listen = fmt.Sprintf("127.0.0.1:%d", edgePort)
	ecfg.PublicURL = fmt.Sprintf("http://127.0.0.1:%d", edgePort)
	ecfg.DataDir = filepath.Join(t.TempDir(), "edge")
	ecfg.ControllerURL = ctlURL
	ecfg.JoinToken = tok.Token
	ecfg.HoldTimeout = 2 * time.Second
	ed := &edge.Edge{Cfg: ecfg, Log: quiet}
	go func() { _ = ed.Run(ctx) }()
	waitHTTP(t, ecfg.PublicURL+"/healthz", 10*time.Second)

	// Wait for both nodes to appear healthy in the registry.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var nodes []protocol.NodeStatus
		api.call(t, http.MethodGet, "/api/v1/nodes", nil, &nodes)
		healthy := 0
		for _, n := range nodes {
			if n.State == "healthy" {
				healthy++
			}
		}
		if healthy == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nodes never became healthy: %+v", nodes)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// --- content: write an HLS stream into the origin's watch dir ---
	streamDir := filepath.Join(watchDir, "live")
	if err := os.MkdirAll(streamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	segData := strings.Repeat("SEGMENT-DATA-", 1000)
	if err := os.WriteFile(filepath.Join(streamDir, "seg0.ts"), []byte(segData), 0o644); err != nil {
		t.Fatal(err)
	}
	playlist := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nseg0.ts\n"
	if err := os.WriteFile(filepath.Join(streamDir, "index.m3u8"), []byte(playlist), 0o644); err != nil {
		t.Fatal(err)
	}

	// --- sign and play through the controller (302 → edge) ---
	var signed protocol.SignResponse
	api.call(t, http.MethodPost, "/api/v1/sign", protocol.SignRequest{Path: "/play/live/index.m3u8"}, &signed)

	client := &http.Client{Timeout: 10 * time.Second} // follows redirects
	var playlistBody string
	deadline = time.Now().Add(10 * time.Second)
	for {
		resp, err := client.Get(ctlURL + signed.URL)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				playlistBody = string(b)
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("playlist never became playable through the router")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(playlistBody, "/play/live/seg0.ts?") {
		t.Fatalf("playlist not rewritten with signed segment URIs:\n%s", playlistBody)
	}

	// --- fetch the segment exactly as a player would ---
	var segURL string
	for _, line := range strings.Split(playlistBody, "\n") {
		if strings.HasPrefix(line, "http") {
			segURL = strings.TrimSpace(line)
			break
		}
	}
	if segURL == "" {
		t.Fatalf("no segment URI in playlist:\n%s", playlistBody)
	}
	resp, err := client.Get(segURL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != segData {
		t.Fatalf("segment playback failed: status=%d len=%d want=%d", resp.StatusCode, len(got), len(segData))
	}

	// --- purge, then verify pull-through refills from origin ---
	api.call(t, http.MethodPost, "/api/v1/purge", protocol.PurgeRequest{Path: "live"}, nil)
	time.Sleep(3 * time.Second) // heartbeat delivers the purge directive

	resp2, err := client.Get(segURL)
	if err != nil {
		t.Fatal(err)
	}
	got2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || string(got2) != segData {
		t.Fatalf("pull-through after purge failed: status=%d len=%d", resp2.StatusCode, len(got2))
	}

	// --- cluster stats should reflect activity ---
	var stats protocol.ClusterStats
	api.call(t, http.MethodGet, "/api/v1/stats", nil, &stats)
	if stats.RoutedTotal < 2 || stats.EdgesTotal != 1 || stats.OriginsTotal != 1 {
		t.Fatalf("stats look wrong: %+v", stats)
	}
}
