package router

import (
	"fmt"
	"testing"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/registry"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
)

func candidates(n int) []registry.RouteCandidate {
	out := make([]registry.RouteCandidate, n)
	for i := range out {
		out[i] = registry.RouteCandidate{
			ID:        fmt.Sprintf("nd_%02d", i),
			PublicURL: fmt.Sprintf("http://edge%d", i),
			Score:     10,
			Healthy:   true,
		}
	}
	return out
}

func settings() protocol.Settings { return protocol.DefaultSettings() }

func TestNoNodes(t *testing.T) {
	r := New()
	if _, err := r.Pick(nil, "s", "", settings()); err != ErrNoNodes {
		t.Fatalf("expected ErrNoNodes, got %v", err)
	}
}

func TestAffinityIsSticky(t *testing.T) {
	r := New()
	cs := candidates(5)
	first, err := r.Pick(cs, "stream1", "", settings())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		d, err := r.Pick(cs, "stream1", "", settings())
		if err != nil {
			t.Fatal(err)
		}
		if d.NodeID != first.NodeID {
			t.Fatalf("affinity broke on iteration %d: %s vs %s", i, d.NodeID, first.NodeID)
		}
	}
}

func TestAffinitySpreadsAcrossStreams(t *testing.T) {
	r := New()
	cs := candidates(8)
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		d, err := r.Pick(cs, fmt.Sprintf("stream%d", i), "", settings())
		if err != nil {
			t.Fatal(err)
		}
		seen[d.NodeID] = true
	}
	// Rendezvous hashing over 64 keys and 8 nodes should touch most nodes.
	if len(seen) < 5 {
		t.Fatalf("streams concentrated on %d/8 nodes — hashing is broken", len(seen))
	}
}

func TestBoundedLoadSpill(t *testing.T) {
	r := New()
	cs := candidates(4)
	winner, _ := r.Pick(cs, "hot-stream", "", settings())

	// Saturate the affinity winner; the request must spill elsewhere.
	for i := range cs {
		if cs[i].ID == winner.NodeID {
			cs[i].Score = 99
		}
	}
	d, err := r.Pick(cs, "hot-stream", "", settings())
	if err != nil {
		t.Fatal(err)
	}
	if d.NodeID == winner.NodeID {
		t.Fatal("saturated affinity winner was not spilled")
	}
	if !d.Spilled {
		t.Error("decision should be marked as spilled")
	}
}

func TestAllSaturatedFallsBackToLeastLoaded(t *testing.T) {
	r := New()
	cs := candidates(3)
	cs[0].Score, cs[1].Score, cs[2].Score = 95, 80, 90
	d, err := r.Pick(cs, "s", "", settings())
	if err != nil {
		t.Fatal(err)
	}
	if d.NodeID != cs[1].ID {
		t.Fatalf("expected least-loaded %s, got %s", cs[1].ID, d.NodeID)
	}
}

func TestDrainingNeverPicked(t *testing.T) {
	r := New()
	cs := candidates(3)
	cs[0].Draining, cs[1].Draining = true, true
	for i := 0; i < 20; i++ {
		d, err := r.Pick(cs, fmt.Sprintf("k%d", i), "", settings())
		if err != nil {
			t.Fatal(err)
		}
		if d.NodeID != cs[2].ID {
			t.Fatalf("draining node picked: %s", d.NodeID)
		}
	}
}

func TestEjectedSkippedUntilPanic(t *testing.T) {
	r := New()
	s := settings()
	s.AffinityEnabled = false

	// 1 of 4 ejected (25% < 50% panic threshold): must be skipped.
	cs := candidates(4)
	cs[0].Ejected = true
	cs[0].Score = 1 // even though it looks best
	for i := 0; i < 10; i++ {
		d, err := r.Pick(cs, "", "", s)
		if err != nil {
			t.Fatal(err)
		}
		if d.NodeID == cs[0].ID {
			t.Fatal("ejected node picked below panic threshold")
		}
	}

	// 3 of 4 ejected (75% > 50%): panic mode routes across all healthy.
	cs = candidates(4)
	cs[0].Ejected, cs[1].Ejected, cs[2].Ejected = true, true, true
	d, err := r.Pick(cs, "", "", s)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Panic {
		t.Error("expected panic-mode decision")
	}
}

func TestRegionPenaltySteersLocal(t *testing.T) {
	r := New()
	s := settings()
	s.AffinityEnabled = false // pure score routing for determinism
	cs := candidates(2)
	cs[0].Region = "us-east"
	cs[1].Region = "eu-west"
	cs[0].Score = 20 // busier…
	cs[1].Score = 5  // …but remote

	d, err := r.Pick(cs, "", "us-east", s)
	if err != nil {
		t.Fatal(err)
	}
	// 20 < 5+30 penalty: local node must win despite higher load.
	if d.NodeID != cs[0].ID {
		t.Fatalf("region steering failed: picked %s", d.NodeID)
	}

	// Under extreme local overload the remote node absorbs traffic.
	cs[0].Score = 99
	d, _ = r.Pick(cs, "", "us-east", s)
	if d.NodeID != cs[1].ID {
		t.Fatalf("regional overload should spill cross-region, picked %s", d.NodeID)
	}
}
