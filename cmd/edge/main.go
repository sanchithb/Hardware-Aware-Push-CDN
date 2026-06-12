package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
)

const (
	VideoRoot     = "/var/cdn/video"
	LoadBalancer  = "http://localhost:8080/update"
	OriginAPI     = "http://localhost:9000/subscribe"
	EdgePublicIP  = "http://127.0.0.1:8081"
)

func registerWithOrigin() {
	payload, _ := json.Marshal(map[string]string{"edge_url": EdgePublicIP})
	for {
		req, _ := http.NewRequest("POST", OriginAPI, bytes.NewBuffer(payload))
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			log.Println("✅ Successfully registered with Origin Server!")
			return
		}
		time.Sleep(3 * time.Second)
	}
}

func startTelemetryLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		cpuPercent, _ := cpu.Percent(0, false)
		vmStat, _ := mem.VirtualMemory()
		metrics := map[string]float64{"cpu": cpuPercent[0], "ram": vmStat.UsedPercent}
		payload, _ := json.Marshal(metrics)

		req, _ := http.NewRequest("POST", LoadBalancer, bytes.NewBuffer(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Edge-IP", "127.0.0.1:8081")

		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}
}

func handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		return
	}
	filePath := strings.TrimPrefix(r.URL.Path, "/ingest/")
	fullPath := filepath.Join(VideoRoot, filePath)
	os.MkdirAll(filepath.Dir(fullPath), os.ModePerm)

	out, err := os.Create(fullPath)
	if err != nil {
		return
	}
	defer out.Close()

	io.Copy(out, r.Body)
	log.Printf("📥 Received and saved: %s", fullPath)
	w.WriteHeader(http.StatusOK)
}

// --- PHASE 5: Hold-And-Serve Viewer Requests ---
func handlePlay(w http.ResponseWriter, r *http.Request) {
	// Extract the requested file: /play/stream1/chunk.ts -> stream1/chunk.ts
	filePath := strings.TrimPrefix(r.URL.Path, "/play/")
	fullPath := filepath.Join(VideoRoot, filePath)

	// Hold-and-Serve Logic: Wait up to 3 seconds for the file to arrive
	maxRetries := 30            // 30 retries
	retryDelay := 100 * time.Millisecond // 100ms per retry (Total 3 seconds)

	for i := 0; i < maxRetries; i++ {
		// Check if file exists
		if _, err := os.Stat(fullPath); err == nil {
			// File exists! Serve it to the user.
			log.Printf("▶️ Serving file to user: %s", filePath)
			http.ServeFile(w, r, fullPath)
			return
		}

		// If it's the very first try and it's missing, log that we are waiting
		if i == 0 {
			log.Printf("⏳ File not here yet, holding connection for: %s", filePath)
		}

		// Wait 100ms and check again
		time.Sleep(retryDelay)
	}

	// If we waited 3 seconds and the Origin never pushed it, give up.
	log.Printf("❌ Timeout waiting for file: %s", filePath)
	http.Error(w, "Stream chunk not available", http.StatusNotFound)
}

func main() {
	os.MkdirAll(VideoRoot, os.ModePerm)

	go registerWithOrigin()
	go startTelemetryLoop()

	http.HandleFunc("/ingest/", handleIngest)
	http.HandleFunc("/play/", handlePlay) // Using our custom handler

	log.Println("Edge Node running on :8081...")
	log.Fatal(http.ListenAndServe(":8081", nil))
}