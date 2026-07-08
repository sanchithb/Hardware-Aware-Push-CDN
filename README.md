<div align="center">
  <h1>hpcdn — Hardware-Aware Push CDN</h1>
  <p>A self-hosted content delivery network for live video and static content.<br>
  One Go binary: control plane, edge caches, origin, CLI and web console.</p>
</div>

---

`hpcdn` lets you run your own CDN on machines you control — cloud VMs,
colo boxes, or a rack of spare hardware. An **origin** watches your
content directory and pushes new files to every **edge** the moment they
land on disk; a **controller** tracks live hardware telemetry from every
node and routes each viewer to the healthiest edge, keeping streams
pinned to warm caches. Content that push misses is healed transparently
by pull-through. Everything is operated from one CLI and one embedded
web console.

```
   viewer ──▶ controller ──302──▶ edge (SIEVE cache) ◀──push── origin (fsnotify)
                 ▲                    │    ▲                      │
                 └──── heartbeats ────┘    └──── pull-through ────┘
```

## Why it's interesting

- **Hardware-aware routing with cache affinity.** Every edge reports CPU,
  RAM, disk and connection telemetry every 2 s. The router combines an
  EWMA-smoothed load score with **rendezvous hashing per stream**
  (consistent-hashing-with-bounded-loads), so the same stream keeps
  hitting the same warm cache — until that node saturates, when traffic
  spills to the next-ranked edge automatically.
- **Push + pull hybrid.** Live segments are pushed with SHA-256 integrity
  checks the instant an encoder writes them (fsnotify + debounce +
  concurrent worker pool). Anything push misses — a new edge, a purge, a
  network blip — is filled on demand from the origin with request
  collapsing, so a thundering herd on a cold object costs one fetch.
- **Failure handling modeled on Envoy.** Active health probes with
  circuit breaking (eject → cooldown → half-open probe → re-admit) and a
  panic threshold: if most of the pool looks dead, health data is assumed
  wrong and routing continues rather than 503ing everyone. Playlists are
  rewritten so every segment request re-routes through the controller —
  an edge dying mid-stream heals at the next segment.
- **SIEVE cache eviction** (NSDI '24) instead of LRU — simpler, less lock
  contention on the read path, better miss ratios on skewed CDN traffic.
- **Real security posture.** HMAC-SHA256 signed playback URLs with expiry
  and path scoping (validated locally at the edge, constant-time); node
  enrollment via revocable join tokens with TTL and use limits; per-node
  secrets; all credentials stored hashed.
- **Observability included.** Prometheus `/metrics` on every role, an SSE
  event stream, live-tunable routing settings, and an embedded web
  console (no separate frontend to deploy) with live telemetry charts,
  node management, enrollment and settings.
- **Zero runtime dependencies.** No Redis, no database, no npm build. One
  static binary per node; controller state is one atomic JSON file.

## Quick start (one machine, five minutes)

```bash
go build -o bin/hpcdn ./cmd/hpcdn        # or: make build

# 1. control plane — prints the admin API key ONCE, keep it
bin/hpcdn controller

# 2. in a new shell:
export HPCDN_API_KEY=hpa_…               # from step 1
bin/hpcdn tokens create --note "first nodes"   # → hpj_…

# 3. an edge and an origin (new shells)
bin/hpcdn edge   --join-token hpj_…
bin/hpcdn origin --join-token hpj_… --watch-dir ./content

# 4. publish something — e.g. simulate a live HLS stream with ffmpeg
mkdir -p content/demo
ffmpeg -re -i input.mp4 -c copy -f hls -hls_time 2 content/demo/index.m3u8
# every segment is pushed to the edge as ffmpeg writes it

# 5. mint a signed playback URL and open it in any HLS player
bin/hpcdn sign /play/demo/index.m3u8
```

Web console: **http://localhost:8080** (sign in with the admin key).
Cluster status from anywhere: `hpcdn status`, `hpcdn nodes list`.

Multi-machine is the same flow — give each node `--public-url` it is
reachable at and point `--controller-url` at the control plane. Nodes
enroll themselves with the join token; the console's **Enroll** page
generates the exact commands.

## CLI

```
hpcdn controller|edge|origin     run a role (YAML config, env, or flags)
hpcdn status                     cluster overview
hpcdn nodes list|get|drain|undrain|remove
hpcdn tokens create|list|revoke  node enrollment tokens
hpcdn sign <path>                mint a signed playback URL
hpcdn purge <prefix>             purge content from every edge
hpcdn settings get|set           live-tune routing (no restart)
hpcdn logs                       recent controller logs
hpcdn bench [--sign <path>]      built-in load generator
```

Every command supports `--json` for scripting; everything the CLI does is
plain REST (see [`docs/API.md`](docs/API.md) and
[`api/openapi.yaml`](api/openapi.yaml)).

## Benchmarks

Measured on a 4-core laptop (AMD Ryzen 5 3500U), 20 s runs, pooled
client — full method and old-prototype comparison in
[`docs/BENCHMARKS.md`](docs/BENCHMARKS.md):

| Scenario | Throughput | p50 | p95 | Errors |
|---|---|---|---|---|
| Routing decisions (HMAC + affinity + 302), 500 VUs | **27,105 req/s** | 15.3 ms | 44.1 ms | 0 |
| Routing decisions, 1,000 VUs | **26,465 req/s** | 34.4 ms | 74.5 ms | 0 |
| Routing decisions, 2,000 VUs | **24,493 req/s** | 70.0 ms | 137.7 ms | 0 |
| End-to-end playback (302 → edge validate → rewrite → serve), 500 VUs | 4,744 req/s | 90.3 ms | 124.4 ms | see notes |

## Documentation

| | |
|---|---|
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | how and why the system is shaped this way |
| [`docs/API.md`](docs/API.md) + [`api/openapi.yaml`](api/openapi.yaml) | REST API guide and OpenAPI 3.0 spec |
| [`docs/BENCHMARKS.md`](docs/BENCHMARKS.md) | numbers, method, old-vs-new comparison |
| [`docs/HANDOFF.md`](docs/HANDOFF.md) | continuation guide: invariants, gotchas, prioritized roadmap |
| [`configs/`](configs) | annotated example YAML for each role |
| [`deployments/`](deployments) | Dockerfile, docker-compose lab cluster, Kubernetes manifests |

## Tests

```bash
go test ./...        # unit + full in-process cluster e2e (~15 s)
go test -short ./... # skip the e2e cluster test
```

Covered: HMAC signing (expiry/scope/tamper), routing (affinity, bounded
loads, drain, ejection, panic mode, region steering), SIEVE eviction and
cache integrity, registry enrollment/ack semantics/persistence, watcher
debounce, edge data path (hold-and-serve, pull-through collapsing, HLS
headers, playlist rewriting), metrics exposition, and an end-to-end test
that boots a real cluster and streams through it.

## Status & scope

This is a working, tested v1 of a self-hosted CDN — honest about what it
is: a single-controller design (see the HA plan and full roadmap in
[`docs/HANDOFF.md`](docs/HANDOFF.md)). In redirect mode a controller
outage only blocks *new* sessions; active viewers keep streaming from
edges. Built and verified on Windows, Linux (containers), single- and
multi-node.
