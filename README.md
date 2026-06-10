# Hardware Aware Push CDN

## 1. Executive Summary
This project is a distributed, hardware-aware content delivery network optimized for ultra-low latency live video streaming. By transitioning from NGINX C modules to a modern **Go (Golang)** microservice architecture, the system safely and efficiently implements an active "push" model. It bypasses standard HTTP polling by utilizing file-system event watchers to aggressively push video chunks (`.ts`) and playlists (`.m3u8`) from the Origin to Edge nodes. Traffic routing is managed by a custom Load Balancer that monitors real-time hardware telemetry to ensure zero frame-drops under heavy load.

---

## 2. High-Level Architecture Flow

1. **The Origin (Source):** A transcoder (e.g., FFmpeg) writes segmented live video (`.ts` and `.m3u8`) to a local directory.
2. **The Origin Watcher:** The Go Origin Service actively monitors this directory. Upon file creation, it immediately pushes the file to all subscribed Edge nodes.
3. **The Edge Nodes (Distributors):** Go Edge Services receive the pushed files, store them locally, and serve them to end-users over standard HTTP.
4. **The Hardware Monitor:** Each Go Edge Service continuously calculates its own CPU, RAM, and Network saturation, reporting a unified "Hardware Score" to the Load Balancer.
5. **The Load Balancer (Traffic Cop):** The Go Load Balancer receives user requests, evaluates the real-time hardware scores of all Edge nodes, and issues an HTTP 302 Redirect to the least-stressed node.

---

## 3. Module Specifications

### 3.1. Origin Service (Replaces `ngx_http_push_origin`)
**Primary Role:** Watch the file system for newly encoded video files and aggressively push them to registered downstream Edge servers.

* **Core Triggers:**
  * Uses `github.com/fsnotify/fsnotify` to detect file system events (`Write`, `Create`) in the designated video root directory.
  * Listens for HTTP POST requests from Edge nodes requesting to subscribe/unsubscribe to specific streams (Replaces custom OEP protocol).
* **Processing Logic:**
  * Maintains a thread-safe registry (`sync.RWMutex`) of connected Edge servers and the streams they are subscribed to.
  * When `fsnotify` detects a completed file write, the service parses the file path.
  * If the file is a Master `.m3u8`, it parses the file for secondary playlist URLs.
* **Outputs / Actions:**
  * Uses Go's `net/http` client to execute concurrent HTTP `PUT` requests, sending the raw file payload to the `put_address` provided by the subscribed Edge servers.

### 3.2. Edge Service (Replaces `ngx_http_push_edge`)
**Primary Role:** Act as the ingest point for Origin pushes and the egress point for end-user video playback.

* **Core Triggers:**
  * End-user HTTP GET requests for video stream directories.
  * Origin server HTTP PUT requests containing video files.
* **Processing Logic:**
  * **On User Request:** Checks if the requested stream is currently active. If not, it sends an HTTP POST to the Origin to subscribe to the stream.
  * **Garbage Collection:** Maintains a timeout mechanism. If no users request a stream for a defined period (e.g., 300 seconds), it sends an HTTP POST to the Origin to unsubscribe, saving bandwidth.
* **Outputs / Actions:**
  * Provides an HTTP endpoint (e.g., `/ingest`) to receive and save PUT payloads from the Origin.
  * Uses `http.FileServer` to serve the local video files to requesting users.

### 3.3. Telemetry Service (Replaces `ngx_http_session_manager`)
**Primary Role:** Continuously monitor the host machine's hardware saturation and report to the Load Balancer. *(Note: This runs as a goroutine within the Edge Service).*

* **Core Triggers:**
  * A Go `time.Ticker` triggers the hardware audit at defined intervals (e.g., every 2 seconds).
* **Processing Logic:**
  * Uses `github.com/shirou/gopsutil` to replace manual Linux `/proc` and `/sys` file parsing.
  * Evaluates CPU usage, RAM/Swap usage, Disk capacity, and Network Interface (TX/RX) bandwidth limits.
  * Calculates a unified "Hardware Score" aggregating these metrics.
* **Outputs / Actions:**
  * Sends an HTTP POST containing a JSON payload with the hardware metrics to the Load Balancer (Replaces custom LBP protocol).
  * Exposes a `/metrics` endpoint in Prometheus format for observability.

### 3.4. Load Balancer Service (Replaces `ngx_http_loadbalancer`)
**Primary Role:** Route new user sessions to the healthiest available Edge node based on real-time hardware telemetry.

* **Core Triggers:**
  * HTTP POST updates from Edge node Telemetry Services.
  * End-user HTTP requests hitting the main routing entry point.
* **Processing Logic:**
  * Maintains a thread-safe map (`map[string]EdgeNode`) of all active Edge peers and their current metrics.
  * **Circuit Breaker:** If an Edge reports metrics exceeding predefined thresholds (e.g., CPU > 80%), it is temporarily flagged as `down`.
  * **Routing Policy (Least Loaded):** Scans the map for healthy nodes and selects the peer with the lowest overall hardware score.
* **Outputs / Actions:**
  * Responds to the end-user with an HTTP 302 Redirect, pointing them to the chosen Edge server's URL.

### 3.5. State Manager (Replaces `ngx_http_stream_push`)
**Primary Role:** Distributed state management using Redis Pub/Sub to coordinate stream availability.

* **Processing Logic:**
  * Replaces the NGINX shared memory queue.
  * Uses `github.com/go-redis/redis` to `PUBLISH` stream requests (Join/Leave) across multiple clusters.
  * Edge nodes use `SUBSCRIBE` to listen for stream availability states (`ACTIVE`, `INACTIVE`) before attempting to serve missing files to users.

---

## 4. Step-by-Step Implementation Guide

### Phase 1: Foundation & Telemetry
1. **Initialize the Project:** Create a new Go module (`go mod init video-cdn`).
2. **Build the Telemetry Package:** Install `gopsutil` (`go get github.com/shirou/gopsutil/v3`). Write a function that retrieves CPU, Memory, and Network I/O, calculates a score, and returns a JSON struct.
3. **Build the Load Balancer Core:** Create an HTTP server. Implement a `/update` endpoint to receive the JSON from the Telemetry package. Store this data in a `sync.RWMutex` protected map.
4. **Implement Routing:** Add a `/play` endpoint to the Load Balancer that reads the map, finds the lowest score, and returns an `http.Redirect`.

### Phase 2: The Edge Ingest & Serve
1. **Build the Edge Web Server:** Create an HTTP server with two main routes:
   * `http.Handle("/stream/", http.StripPrefix("/stream/", http.FileServer(http.Dir("/var/cdn/video"))))` to serve files.
   * `http.HandleFunc("/ingest", handleIngest)` to accept HTTP PUT requests.
2. **Handle Ingest:** In `handleIngest`, read the `http.Request.Body` and write it to the local disk at `/var/cdn/video`. Ensure directory creation handles nested paths.
3. **Integrate Telemetry:** Start the Telemetry function as a goroutine (`go startTelemetryLoop()`) when the Edge server boots, pointing it to the Load Balancer's `/update` endpoint.

### Phase 3: The Origin Push Engine
1. **Setup fsnotify:** Install `fsnotify` (`go get github.com/fsnotify/fsnotify`). Initialize a watcher on your encoder's output directory.
2. **Edge Registry:** Create a simple HTTP endpoint on the Origin (`/subscribe`) where Edge nodes can register their IP addresses.
3. **The Push Worker:** When `fsnotify` triggers a `Write` event, place the file path into a Go channel. Create a worker pool of goroutines that read from this channel, read the file from disk, and perform an `http.NewRequest("PUT", ...)` to every registered Edge node concurrently.

### Phase 4: Modernization & Polish
1. **gRPC Transition:** Replace the HTTP REST endpoints between the internal services (Telemetry updates, Edge subscriptions) with gRPC for better performance and strict typing.
2. **Prometheus Metrics:** Import `github.com/prometheus/client_golang/prometheus`. Expose a `/metrics` route on all services to allow Grafana to scrape internal system health.
3. **Dockerization:** Write a `Dockerfile` for each of the three services (Origin, Edge, Load Balancer) to ensure they can be easily deployed via Docker Compose or Kubernetes.
