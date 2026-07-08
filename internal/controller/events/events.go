// Package events is a small in-process pub/sub hub used to fan controller
// events out to SSE subscribers (web console, CLI watchers).
package events

import (
	"sync"
	"time"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
)

// Hub broadcasts events to subscribers and retains a short replay buffer.
type Hub struct {
	mu     sync.Mutex
	subs   map[chan protocol.Event]struct{}
	recent []protocol.Event
	max    int
}

// NewHub creates a hub retaining up to replay recent events.
func NewHub(replay int) *Hub {
	if replay <= 0 {
		replay = 100
	}
	return &Hub{subs: map[chan protocol.Event]struct{}{}, max: replay}
}

// Publish sends an event to all subscribers; slow subscribers are skipped
// rather than blocking the control plane.
func (h *Hub) Publish(typ, node, msg string) {
	ev := protocol.Event{Time: time.Now().UTC(), Type: typ, Node: node, Msg: msg}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recent = append(h.recent, ev)
	if len(h.recent) > h.max {
		h.recent = h.recent[len(h.recent)-h.max:]
	}
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe registers a new subscriber and returns its channel plus a
// snapshot of recent events for immediate replay.
func (h *Hub) Subscribe() (ch chan protocol.Event, replay []protocol.Event, cancel func()) {
	ch = make(chan protocol.Event, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	replay = append(replay, h.recent...)
	h.mu.Unlock()
	return ch, replay, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}
