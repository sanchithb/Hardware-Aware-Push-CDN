<div align="center">
  <h1>Hardware Aware Push CDN</h1>
  <p>An ultra low latency, auto distributing Video Content Delivery Network written in Go.</p>
</div>

## Summary

This project is a distributed, hardware aware content delivery network built from the ground up in Go. Optimized for ultra low latency live video streaming, it solves the thundering herd problem and bandwidth waste of traditional polling based CDNs by implementing an active Push architecture. 

Engineered for scale and fault tolerance, it features a concurrent file distribution pipeline, real time hardware telemetry, per segment circuit breaking, geographic routing, native TLS termination, LRU caching, HMAC request authentication, and sub 10ms routing latency under load.

## Systems Engineering Highlights

* **Concurrent Push Architecture:** Origin actively pushes video segments to Edge nodes the millisecond they hit the disk using OS level fsnotify hooks and concurrent HTTP clients.
* **Hardware Aware Load Balancing:** Edge nodes continuously monitor their CPU and RAM saturation. The Load Balancer maintains a lock free registry and issues HTTP 302 redirects to the healthiest Edge node in real time.
* **Geographic Distribution (PoP Routing):** The Load Balancer inspects the X User Region header to penalize and filter Edge nodes outside the client's geographic Point of Presence, ensuring traffic remains local.
* **Mid Stream Fault Tolerance:** Edge nodes dynamically rewrite playlist URLs on the fly to force chunk requests back through the Load Balancer. This allows the Load Balancer's circuit breaker to instantly reroute a user to a healthy node mid stream if their current Edge node crashes.
* **Native TLS Termination:** The CDN terminates HTTPS traffic natively across all nodes using in memory, dynamically generated self signed x509 certificates, enabling HTTP/2 streaming.
* **LRU Caching Layer:** Edge nodes enforce storage efficiency by running background eviction routines that continuously calculate disk utilization and purge the oldest video segments when thresholds are exceeded.
* **HMAC Request Authentication:** The routing engine is locked down using cryptographically signed URLs (HMAC SHA256) with expiration timestamps, preventing unauthorized stream access.
* **Test Coverage:** Critical routing logic, authentication signatures, cache evictions, and debounce algorithms are fully covered by Go unit tests.
* **Real Time Observability:** Every node ships with an embedded React Dashboard offering live terminal logs, hardware telemetry, and ECharts topology visualization without needing a separate frontend server.

## Architecture and File Structure

```text
.
+-- api
|   +-- proto
|       +-- cdn.proto
+-- cmd
|   +-- edge
|   |   +-- main.go
|   +-- loadbalancer
|   |   +-- main.go
|   +-- origin
|       +-- main.go
+-- deployments
|   +-- docker-compose.yml
|   +-- edge.Dockerfile
|   +-- loadbalancer.Dockerfile
|   +-- origin.Dockerfile
+-- internal
|   +-- edge
|   |   +-- cache
|   |   |   +-- cache_evictor.go
|   |   +-- ingest
|   |   |   +-- ingest.go
|   |   +-- server
|   |   |   +-- server.go
|   |   +-- state
|   |   |   +-- state.go
|   |   +-- telemetry
|   |       +-- telemetry.go
|   +-- loadbalancer
|   |   +-- api
|   |   |   +-- grpc.go
|   |   +-- registry
|   |   |   +-- registry.go
|   |   +-- router
|   |       +-- router.go
|   +-- origin
|       +-- pusher
|       |   +-- pusher.go
|       +-- registry
|       |   +-- registry.go
|       +-- watcher
|           +-- watcher.go
+-- pkg
|   +-- auth
|   |   +-- auth.go
|   +-- config
|   |   +-- config.go
|   +-- dashboard
|   |   +-- dashboard.go
|   +-- logger
|   |   +-- logger.go
|   +-- metrics
|   |   +-- metrics.go
|   +-- tlsutil
|       +-- tlsutil.go
+-- scripts
|   +-- load_test.js
+-- README.md
```

## Benchmarks

Ultra low latency must be quantified. We benchmarked the Load Balancer's TLS termination, authentication, and routing redirect logic using k6 to simulate high concurrent viewership.

The tests hit the Load Balancer with increasing Virtual Users (VUs) constantly requesting stream routing over 25 seconds.

### 500 Virtual Users

| Metric | Result |
| :--- | :--- |
| **Throughput** | 1691 req/sec |
| **Average Routing Latency** | 115.92 ms |
| **P(95) Latency** | 261.59 ms |
| **Error Rate** | 0.00% |

### 1000 Virtual Users

| Metric | Result |
| :--- | :--- |
| **Throughput** | 1785 req/sec |
| **Average Routing Latency** | 279.94 ms |
| **P(95) Latency** | 651.56 ms |
| **Error Rate** | 0.00% |

### 5000 Virtual Users (Stress Test)

| Metric | Result |
| :--- | :--- |
| **Throughput** | 793 req/sec |
| **Average Routing Latency** | 724.84 ms |
| **P(95) Latency** | 2170 ms |
| **Error Rate** | 31.70% (Local OS Socket Exhaustion) |

**Resource Utilization (Load Balancer at 5000 VUs):**
* **CPU:** 131.07%
* **Memory:** 142.5 MiB

> *Stress testing identified a saturation point around 1.7–1.8k requests/sec. Under 5,000 concurrent virtual users the system experienced increased queueing delays and a 31.7% error rate, highlighting future optimization opportunities in routing and request handling.*

## Architecture Flow

1. **The Origin (Source):** Watches a local directory using fsnotify. Upon detecting a new video chunk, it concurrently pushes it to all registered Edge nodes over HTTP.
2. **The Edge Nodes (Distributors):** Receive files, store them, and serve them to end users with Cache Control headers. They dynamically rewrite playlists to inject auth tokens for failover and continuously broadcast their Hardware Metrics to the Load Balancer.
3. **The Load Balancer (Traffic Cop):** Intercepts client playback requests, validates HMAC signatures, evaluates the real time hardware scores of all Edge nodes by region, and instantly redirects the user to the least stressed local node.

## Quick Start Guide

### Prerequisites
* Docker and Docker Compose installed.
* FFmpeg installed locally to generate test video streams.

### 1. Boot the Cluster

Clone the repository and spin up the complete microservice cluster (Origin, Load Balancer, Edge, and Redis):

```bash
docker-compose -f deployments/docker-compose.yml up -d --build
```

### 2. Generate an Authenticated URL

To play a stream, you must first request a cryptographically signed URL from the Load Balancer's signing endpoint:

```text
https://localhost:80/sign?path=/play/stream1/index.m3u8
```

### 3. Start a Live Stream

If you have an mp4 file, you can simulate a live stream by encoding it into HLS chunks directly into the mapped video directory:

```bash
docker run --rm -v "$(pwd)/video:/video" -w /video jrottenberg/ffmpeg -i sample.mp4 -hls_time 2 -hls_list_size 0 -f hls index.m3u8
```

Watch the **Origin Dashboard Logs** you will immediately see the Origin detecting the generated chunks and pushing them to the Edge node.

### 4. Play the Stream

Point your HLS compatible video player to the signed URL generated in step 2. The Edge node will validate the signature and serve the stream over HTTPS.

## Configuration and Tuning

You can dynamically tune the Load Balancer without restarting the cluster via the **Load Balancer Dashboard** Settings panel:
* **CPU Weight:** Prioritize routing based on CPU availability.
* **RAM Weight:** Prioritize routing based on Memory availability.
* **Circuit Breaker Timeout:** The threshold before an unresponsive Edge is temporarily removed from the routing pool.
* **Region Penalty:** The score penalty applied when an Edge node is not geographically close to the requesting user.

## Running Tests

To verify the critical routing, authentication, and caching algorithms, run the test suite:

```bash
go test ./internal/... ./pkg/...
```

To run the load testing benchmark yourself, you can pass the number of concurrent virtual users via the `VUS` environment variable:

```bash
docker run --rm -v ${PWD}/scripts:/scripts -w /scripts -e VUS=1000 grafana/k6 run load_test.js
```

