package registry

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/events"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/store"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
)

func newTestRegistry(t *testing.T) (*Registry, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Mutate(func(s *store.State) {
		s.SigningKey = "hps_k"
		s.IngestKey = "hpi_k"
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(st, events.NewHub(10), DefaultConfig(), log), st
}

func enroll(t *testing.T, r *Registry, kind protocol.NodeKind, name string) protocol.RegisterResponse {
	t.Helper()
	tok, err := r.CreateToken("t", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.Register(protocol.RegisterRequest{
		JoinToken: tok.Token, Kind: kind, Name: name,
		PublicURL: "http://" + name + ":1", Capacity: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestEnrollmentIssuesIdentityAndKeys(t *testing.T) {
	r, _ := newTestRegistry(t)
	resp := enroll(t, r, protocol.KindEdge, "e1")
	if resp.NodeID == "" || resp.NodeSecret == "" {
		t.Fatal("missing identity")
	}
	if resp.SigningKey != "hps_k" || resp.IngestKey != "hpi_k" {
		t.Fatal("cluster keys not distributed")
	}
}

func TestBadTokenRejected(t *testing.T) {
	r, _ := newTestRegistry(t)
	_, err := r.Register(protocol.RegisterRequest{
		JoinToken: "hpj_wrong", Kind: protocol.KindEdge, PublicURL: "http://x",
	})
	if err != ErrBadToken {
		t.Fatalf("expected ErrBadToken, got %v", err)
	}
}

func TestTokenMaxUsesEnforced(t *testing.T) {
	r, _ := newTestRegistry(t)
	tok, _ := r.CreateToken("single", 0, 1)
	req := protocol.RegisterRequest{JoinToken: tok.Token, Kind: protocol.KindEdge, PublicURL: "http://a"}
	if _, err := r.Register(req); err != nil {
		t.Fatalf("first use failed: %v", err)
	}
	if _, err := r.Register(req); err != ErrBadToken {
		t.Fatalf("second use of single-use token allowed: %v", err)
	}
}

func TestHeartbeatAuthAndScoring(t *testing.T) {
	r, _ := newTestRegistry(t)
	resp := enroll(t, r, protocol.KindEdge, "e1")

	if _, err := r.Heartbeat(resp.NodeID, "hpn_wrong", protocol.Heartbeat{}); err == nil {
		t.Fatal("wrong secret accepted")
	}
	_, err := r.Heartbeat(resp.NodeID, resp.NodeSecret, protocol.Heartbeat{
		CPUPercent: 50, RAMPercent: 50, ActiveConns: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	n, _ := r.Node(resp.NodeID)
	// score = 50*0.5 + 50*0.2 + (50/100*100)*0.3 = 25+10+15 = 50
	if n.Score < 49 || n.Score > 51 {
		t.Fatalf("unexpected composite score %f", n.Score)
	}
	if n.State != "healthy" {
		t.Fatalf("state = %s", n.State)
	}
}

func TestDirectiveQueueAckSemantics(t *testing.T) {
	r, _ := newTestRegistry(t)
	resp := enroll(t, r, protocol.KindEdge, "e1")
	_, _ = r.Heartbeat(resp.NodeID, resp.NodeSecret, protocol.Heartbeat{})

	r.Purge("/stream1")
	hb, err := r.Heartbeat(resp.NodeID, resp.NodeSecret, protocol.Heartbeat{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hb.Directives) != 1 || hb.Directives[0].Type != protocol.DirectivePurge {
		t.Fatalf("purge directive not delivered: %+v", hb.Directives)
	}
	seq := hb.Directives[0].Seq

	// Unacked directives are redelivered (at-least-once)…
	hb2, _ := r.Heartbeat(resp.NodeID, resp.NodeSecret, protocol.Heartbeat{})
	if len(hb2.Directives) != 1 {
		t.Fatal("directive dropped before ack")
	}
	// …and acked ones are not.
	hb3, _ := r.Heartbeat(resp.NodeID, resp.NodeSecret, protocol.Heartbeat{AckSeq: seq})
	if len(hb3.Directives) != 0 {
		t.Fatalf("acked directive redelivered: %+v", hb3.Directives)
	}
}

func TestTopologyExchange(t *testing.T) {
	r, _ := newTestRegistry(t)
	e := enroll(t, r, protocol.KindEdge, "e1")
	o := enroll(t, r, protocol.KindOrigin, "o1")
	_, _ = r.Heartbeat(e.NodeID, e.NodeSecret, protocol.Heartbeat{})
	_, _ = r.Heartbeat(o.NodeID, o.NodeSecret, protocol.Heartbeat{})

	ohb, _ := r.Heartbeat(o.NodeID, o.NodeSecret, protocol.Heartbeat{})
	if len(ohb.Edges) != 1 || ohb.Edges[0].IngestURL != "http://e1:1/ingest" {
		t.Fatalf("origin did not learn edges: %+v", ohb.Edges)
	}
	ehb, _ := r.Heartbeat(e.NodeID, e.NodeSecret, protocol.Heartbeat{})
	if len(ehb.Origins) != 1 || ehb.Origins[0].FetchURL != "http://o1:1/fetch" {
		t.Fatalf("edge did not learn origins: %+v", ehb.Origins)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(dir)
	_ = st.Mutate(func(s *store.State) { s.SigningKey = "hps_k"; s.IngestKey = "hpi_k" })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := New(st, events.NewHub(10), DefaultConfig(), log)
	tok, _ := r.CreateToken("t", 0, 0)
	resp, err := r.Register(protocol.RegisterRequest{
		JoinToken: tok.Token, Kind: protocol.KindEdge, Name: "e1", PublicURL: "http://e1:1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate controller restart: reopen the store, rebuild the registry.
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r2 := New(st2, events.NewHub(10), DefaultConfig(), log)
	if _, err := r2.Heartbeat(resp.NodeID, resp.NodeSecret, protocol.Heartbeat{}); err != nil {
		t.Fatalf("node identity lost across restart: %v", err)
	}
}

func TestRatesDerivedFromCumulativeCounters(t *testing.T) {
	r, _ := newTestRegistry(t)
	resp := enroll(t, r, protocol.KindEdge, "e1")
	_, _ = r.Heartbeat(resp.NodeID, resp.NodeSecret, protocol.Heartbeat{BytesOut: 0})
	time.Sleep(50 * time.Millisecond)
	_, _ = r.Heartbeat(resp.NodeID, resp.NodeSecret, protocol.Heartbeat{BytesOut: 1_000_000})
	n, _ := r.Node(resp.NodeID)
	if n.BytesOutRate <= 0 {
		t.Fatal("bytes-out rate not derived")
	}
}
