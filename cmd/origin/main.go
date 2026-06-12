package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	WatchDir   = "/var/cdn/origin_encoded"
	OriginPort = ":9000"
)

// Registry for Edge nodes
type EdgeRegistry struct {
	mu    sync.RWMutex
	nodes map[string]bool
}

var registry = &EdgeRegistry{
	nodes: make(map[string]bool),
}

// Debouncer to prevent fsnotify spam
var (
	debounceMu sync.Mutex
	lastPushed = make(map[string]time.Time)
)

type SubscribeRequest struct {
	EdgeURL string `json:"edge_url"`
}

func handleSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		return
	}
	var req SubscribeRequest
	json.NewDecoder(r.Body).Decode(&req)

	registry.mu.Lock()
	registry.nodes[req.EdgeURL] = true
	registry.mu.Unlock()

	log.Printf("✅ Edge registered successfully: %s", req.EdgeURL)
	w.WriteHeader(http.StatusOK)
}

func pushFileToEdges(filePath string) {
	// --- FIX: DEBOUNCING LOGIC ---
	debounceMu.Lock()
	lastTime, exists := lastPushed[filePath]
	if exists && time.Since(lastTime) < 500*time.Millisecond {
		debounceMu.Unlock()
		return // Skip if we just pushed this file a few milliseconds ago
	}
	lastPushed[filePath] = time.Now()
	debounceMu.Unlock()
	// -----------------------------

	fileContent, err := os.ReadFile(filePath)
	if err != nil {
		return
	}

	relPath, _ := filepath.Rel(WatchDir, filePath)
	relPath = strings.ReplaceAll(relPath, "\\", "/")

	registry.mu.RLock()
	var edges []string
	for edgeURL := range registry.nodes {
		edges = append(edges, edgeURL)
	}
	registry.mu.RUnlock()

	for _, edgeURL := range edges {
		go func(targetEdge string) {
			targetURL := targetEdge + "/ingest/" + relPath
			req, _ := http.NewRequest(http.MethodPut, targetURL, bytes.NewReader(fileContent))
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				log.Printf("🚀 Successfully pushed: %s to %s", relPath, targetEdge)
				resp.Body.Close()
			}
		}(edgeURL)
	}
}

func main() {
	os.MkdirAll(WatchDir, os.ModePerm)

	go func() {
		http.HandleFunc("/subscribe", handleSubscribe)
		log.Printf("Origin Subscription API running on %s", OriginPort)
		log.Fatal(http.ListenAndServe(OriginPort, nil))
	}()

	watcher, _ := fsnotify.NewWatcher()
	defer watcher.Close()

	filepath.Walk(WatchDir, func(path string, info os.FileInfo, err error) error {
		if info != nil && info.IsDir() {
			watcher.Add(path)
			log.Printf("👀 Watching directory: %s", path)
		}
		return nil
	})

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok { return }
				
				if event.Op&fsnotify.Create == fsnotify.Create {
					info, err := os.Stat(event.Name)
					if err == nil && info.IsDir() {
						watcher.Add(event.Name)
						log.Printf("👀 Started watching NEW directory: %s", event.Name)
					}
				}

				if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
					info, err := os.Stat(event.Name)
					if err == nil && !info.IsDir() {
						if strings.HasSuffix(event.Name, ".ts") || strings.HasSuffix(event.Name, ".m3u8") {
							pushFileToEdges(event.Name)
						}
					}
				}
			}
		}
	}()

	log.Printf("Origin Node running... actively watching: %s", WatchDir)
	select {}
}