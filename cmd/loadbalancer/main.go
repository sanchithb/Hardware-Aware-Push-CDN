package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

// HardwareMetrics matches the JSON sent by the Edge Telemetry
type HardwareMetrics struct {
	CPUPercent float64 `json:"cpu"`
	RAMPercent float64 `json:"ram"`
	LastSeen   time.Time
}

// EdgeRegistry holds the state of all connected Edge nodes safely
type EdgeRegistry struct {
	mu    sync.RWMutex
	nodes map[string]HardwareMetrics
}

var registry = &EdgeRegistry{
	nodes: make(map[string]HardwareMetrics),
}

// 1. Telemetry Ingest: Edges POST their metrics here
func handleUpdateMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var metrics HardwareMetrics
	if err := json.NewDecoder(r.Body).Decode(&metrics); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	metrics.LastSeen = time.Now()

	// The Edge should send its IP in a header or we take it from RemoteAddr
	edgeIP := r.Header.Get("X-Edge-IP") 
	if edgeIP == "" {
		edgeIP = r.RemoteAddr
	}

	registry.mu.Lock()
	registry.nodes[edgeIP] = metrics
	registry.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

// 2. The Router: Redirects user to the Least Loaded Edge
func handlePlay(w http.ResponseWriter, r *http.Request) {
	streamPath := r.URL.Path // e.g., /play/stream1/index.m3u8

	registry.mu.RLock()
	defer registry.mu.RUnlock()

	if len(registry.nodes) == 0 {
		http.Error(w, "No healthy edge servers available", http.StatusServiceUnavailable)
		return
	}

	var bestEdge string
	lowestScore := 999.0

	// "Least Loaded" Routing Policy
	for ip, metrics := range registry.nodes {
		// Calculate a hardware score (Weight CPU heavily)
		score := (metrics.CPUPercent * 0.7) + (metrics.RAMPercent * 0.3)

		// Circuit Breaker: Ignore dead nodes (no heartbeat in 10s)
		if time.Since(metrics.LastSeen) > 10*time.Second {
			continue
		}

		if score < lowestScore {
			lowestScore = score
			bestEdge = ip
		}
	}

	if bestEdge == "" {
		http.Error(w, "All edge servers are currently overloaded/dead", http.StatusServiceUnavailable)
		return
	}

	// 302 Redirect user to the winning Edge node
	redirectURL := "http://" + bestEdge + streamPath
	log.Printf("Redirecting viewer to: %s (Score: %.2f)", redirectURL, lowestScore)
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func main() {
	http.HandleFunc("/update", handleUpdateMetrics)
	http.HandleFunc("/play/", handlePlay) // Route streams

	log.Println("Load Balancer running on :8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}