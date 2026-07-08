// Package router selects the edge node for a playback request. The
// algorithm layers three production techniques:
//
//  1. Cache affinity via rendezvous (highest-random-weight) hashing on the
//     stream key, so requests for the same stream keep landing on the same
//     edge and its cache stays hot — the same idea behind Fastly's POP
//     clustering and Cloudflare's tiered-cache consistent hashing.
//  2. Bounded loads: the affinity winner is only used while its
//     EWMA-smoothed hardware score is below the saturation threshold;
//     beyond it the request "spills" to the next node in rendezvous order
//     (consistent hashing with bounded loads, Mirrokni et al. 2017).
//  3. Envoy-style panic threshold: if more than PanicThreshold of the pool
//     is ejected/unhealthy, health states are ignored — routing to a
//     possibly degraded node beats returning 503 to everyone.
//
// Region awareness is a score penalty rather than a hard filter, so local
// nodes win under normal load but remote nodes absorb regional overload.
package router

import (
	"errors"
	"hash/maphash"
	"math"
	"sort"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller/registry"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
)

// ErrNoNodes means no edge can take the request right now.
var ErrNoNodes = errors.New("router: no eligible edge nodes")

// Decision describes a routing outcome, kept for logging/metrics.
type Decision struct {
	NodeID    string
	PublicURL string
	Score     float64 // effective score including region penalty
	Panic     bool    // panic-mode routing was active
	Spilled   bool    // affinity winner was saturated, spilled over
}

// Router is stateless besides its hash seed; candidates and settings are
// supplied per call so it always routes on live data.
type Router struct {
	seed maphash.Seed
}

// New creates a Router.
func New() *Router {
	return &Router{seed: maphash.MakeSeed()}
}

// rendezvousRank scores a (node, key) pair; higher wins.
func (r *Router) rendezvousRank(nodeID, key string) uint64 {
	var h maphash.Hash
	h.SetSeed(r.seed)
	h.WriteString(nodeID)
	h.WriteByte(0)
	h.WriteString(key)
	return h.Sum64()
}

// effectiveScore applies the region penalty to a candidate's EWMA score.
func effectiveScore(c registry.RouteCandidate, clientRegion string, s protocol.Settings) float64 {
	score := c.Score
	if clientRegion != "" && c.Region != "" && c.Region != clientRegion {
		score += s.RegionPenalty
	}
	return score
}

// Pick chooses an edge for streamKey (typically the stream's directory
// path) requested from clientRegion ("" if unknown).
func (r *Router) Pick(candidates []registry.RouteCandidate, streamKey, clientRegion string, s protocol.Settings) (Decision, error) {
	if len(candidates) == 0 {
		return Decision{}, ErrNoNodes
	}

	// Partition by usability. Draining nodes never take new sessions.
	usable := make([]registry.RouteCandidate, 0, len(candidates))
	unhealthy := 0
	for _, c := range candidates {
		if c.Draining {
			continue
		}
		if !c.Healthy || c.Ejected {
			unhealthy++
			continue
		}
		usable = append(usable, c)
	}

	poolSize := len(usable) + unhealthy
	panicMode := false
	if poolSize > 0 && s.PanicThreshold > 0 &&
		float64(unhealthy)/float64(poolSize) > s.PanicThreshold {
		// Too much of the pool is ejected for the health data to be
		// trustworthy — route across everything not draining.
		panicMode = true
		usable = usable[:0]
		for _, c := range candidates {
			if !c.Draining && c.Healthy {
				usable = append(usable, c)
			}
		}
	}
	if len(usable) == 0 {
		return Decision{}, ErrNoNodes
	}

	// Order by rendezvous rank for cache affinity, or by score when
	// affinity is disabled.
	if s.AffinityEnabled && streamKey != "" {
		sort.Slice(usable, func(i, j int) bool {
			return r.rendezvousRank(usable[i].ID, streamKey) > r.rendezvousRank(usable[j].ID, streamKey)
		})
	} else {
		sort.Slice(usable, func(i, j int) bool {
			return effectiveScore(usable[i], clientRegion, s) < effectiveScore(usable[j], clientRegion, s)
		})
	}

	// Bounded load: first node in preference order below saturation wins.
	sat := s.SaturationScore
	if sat <= 0 {
		sat = 75
	}
	spilled := false
	for i, c := range usable {
		if effectiveScore(c, clientRegion, s) < sat {
			return Decision{
				NodeID: c.ID, PublicURL: c.PublicURL,
				Score: effectiveScore(c, clientRegion, s),
				Panic: panicMode, Spilled: spilled && i > 0,
			}, nil
		}
		spilled = true
	}

	// Everyone is saturated: degrade gracefully to global least-loaded.
	best := usable[0]
	bestScore := math.Inf(1)
	for _, c := range usable {
		if sc := effectiveScore(c, clientRegion, s); sc < bestScore {
			best, bestScore = c, sc
		}
	}
	return Decision{
		NodeID: best.ID, PublicURL: best.PublicURL,
		Score: bestScore, Panic: panicMode, Spilled: true,
	}, nil
}
