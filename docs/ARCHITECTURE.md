# Architecture

hpcdn is a self-hosted CDN with a clean control-plane / data-plane split.
One binary (`hpcdn`) runs three roles; everything else — CLI, web console,
metrics — rides on the controller's HTTP surface.

```
                            ┌─────────────────────────────┐
   admin / CI / console ───►│  CONTROLLER  (control plane) │
                            │  registry · router · tokens  │
                            │  settings · SSE · metrics    │
                            └───────┬──────────────▲──────┘
              302 redirect          │              │ heartbeats (2s)
        ┌───────────────────────────┘              │ directives + topology
        ▼                                          │
   ┌─────────┐   push (PUT /ingest, SHA-256)  ┌────┴────┐
   │  EDGE   │◄────────────────────────────── │ ORIGIN  │
   │  cache  │ ────────────────────────────►  │ watcher │
   └────▲────┘   pull-through (GET /fetch)    └─────────┘
        │ signed playback (HMAC)
      viewer
```

## Roles

### Controller (control plane)
- **Registry** (`internal/controller/registry`): node enrollment via
  hashed join tokens, per-node secrets, heartbeat ingestion, EWMA-smoothed
  composite load scores, telemetry history rings for the console, and a
  per-node directive queue (purge/drain) with seq/ack at-least-once
  delivery piggybacked on heartbeat responses — so nodes never need an
  inbound control connection.
- **Active health prober**: GETs every edge's `/healthz` on an interval;
  N consecutive failures opens the circuit (node ejected from routing),
  a half-open probe after a cooldown closes it. Modeled on Envoy outlier
  detection, including the **panic threshold**: if more than half the pool
  is ejected, health data is assumed wrong and routing continues anyway.
- **Router** (`internal/controller/router`): stateless per-request
  selection. Preference order is rendezvous (HRW) hashing on the stream
  key — cache affinity, same idea as Fastly POP clustering — walked in
  rank order until a node under the saturation score is found
  (consistent-hashing-with-bounded-loads). Region mismatch is a score
  *penalty*, not a filter, so local nodes win normally and remote nodes
  absorb regional overload. Falls back to least-loaded when everyone is
  saturated.
- **Store** (`internal/controller/store`): one JSON document, atomic
  temp+rename writes. Credentials are stored **hashed** (admin key, join
  tokens, node secrets); the signing/ingest cluster keys are stored raw
  because they must be distributed to nodes.
- **API** (`internal/controller/api`): REST under `/api/v1` (see
  `docs/API.md` / `api/openapi.yaml`), SSE event stream, Prometheus
  `/metrics`, and the public `/play/*` routing endpoint (302 or reverse
  proxy, config `routing_mode`).

### Edge (data plane)
- **Cache** (`internal/edge/cache`): disk-backed, byte-bounded, **SIEVE
  eviction** (NSDI'24) — FIFO order, visited bit, sweeping hand; simpler
  than LRU with better miss ratios on skewed CDN workloads. Index is
  rebuilt from disk on restart. All writes go through temp+rename with
  optional SHA-256 verification, so a partially-written object is never
  visible.
- **Serve** (`internal/edge/serve`): the viewer path.
  - HMAC signature validation happens **locally** (edges receive the
    cluster signing key at enrollment) — no controller round trip per
    segment.
  - HLS-correct headers: playlists get short TTLs (default 2 s),
    segments `immutable` long TTLs; uniform CORS; byte ranges and
    conditionals via `http.ServeContent`.
  - **Hold-and-serve**: a request for a segment that hasn't arrived yet
    parks on an arrival channel while, in parallel, a **singleflight
    pull-through** fetches it from an origin. Whichever fills the cache
    first wins; a thundering herd on a cold object costs exactly one
    origin fetch.
  - **Playlist rewriting** injects the request's validated signature into
    every URI (including `URI="…"` attributes), pointing them back
    through the controller — so each segment request is re-routed and a
    mid-stream edge failure heals on the next segment.
- **Drain**: a drained edge is excluded from routing and answers new
  plays 503 while existing transfers finish.

### Origin
- **Watcher** (`internal/origin/watcher`): recursive fsnotify with
  trailing-edge debounce per file (a timer-identity check makes concurrent
  fire/reset races emit exactly once), temp-file filtering, auto-watch of
  new subdirectories.
- **Pusher** (`internal/origin/pusher`): bounded worker pool; each file
  fans out to every edge concurrently with SHA-256 integrity headers and
  exponential-backoff retries. Push failures are safe: the object stays
  reachable via pull-through.
- **/fetch**: authenticated (cluster ingest key) file server backing edge
  pull-through — also the path by which brand-new edges warm up on
  content that predates them.

### Shared node machinery (`internal/nodeagent`)
Enrollment (join token → persisted identity), heartbeat loop with gopsutil
telemetry, directive dispatch, topology consumption. Both edge and origin
embed it; a future node role would too.

## Security model

| Credential | Prefix | Stored | Purpose |
|---|---|---|---|
| Admin API key | `hpa_` | hashed | CLI/console/API access |
| Join token | `hpj_` | hashed | one-time node enrollment; TTL + max-uses |
| Node secret | `hpn_` | hashed | authenticates heartbeats |
| Signing key | `hps_` | raw (distributed) | HMAC-SHA256 playback URLs |
| Ingest key | `hpi_` | raw (distributed) | origin→edge push, edge→origin pull |

Playback URLs carry `hpe` (expiry), optional `hps` (path scope) and `hpx`
(signature over `scope\nexpiry`). Scoping to the stream directory lets one
token cover a playlist and all its segments. Validation uses
constant-time compares everywhere.

TLS: every role serves HTTPS with provided certs or an in-memory
self-signed cert (`pkg/tlsutil`); production deployments are expected to
terminate TLS at real certs or an ingress.

## Design decisions worth knowing

1. **Pull-based control channel.** Directives ride heartbeat *responses*.
   Nodes behind NAT need no inbound connectivity from the controller
   (except `/healthz`, used only for circuit breaking — nodes that can't
   accept it still work, they just don't get outlier ejection).
2. **Push + pull hybrid.** Push gives live streams their latency;
   pull-through gives correctness when push fails, an edge joins late, or
   content predates an edge. The prototype was push-only and dropped
   content on any miss.
3. **Redirect-first routing.** 302s keep the controller out of the media
   path entirely — the measured routing decision costs ~15 ms p50 at 500
   concurrent users on a laptop. Proxy mode exists for single-hostname
   deployments where redirect targets aren't reachable.
4. **No external dependencies at runtime.** State is a JSON file, metrics
   are hand-rolled exposition format, the console is embedded static
   files. The whole system is one binary per node. (The prototype's
   README referenced Redis; nothing here needs it.)
5. **Everything live-tunable.** Routing weights, saturation, penalties,
   EWMA alpha, TTLs — `PUT /api/v1/settings`, applied on the next request.

## Repository layout

```
cmd/hpcdn/                 CLI entry point
internal/cli/              cobra command tree (run roles + admin client)
internal/protocol/         wire types shared by controller and nodes
internal/controller/       control plane (api, registry, router, store, events)
internal/nodeagent/        enrollment + heartbeat loop shared by node roles
internal/edge/             cache (SIEVE), serve (data plane), wiring
internal/origin/           watcher, pusher, /fetch, wiring
internal/client/           typed Go client for the admin API (CLI uses it)
internal/bench/            built-in load generator
internal/e2e/              full-cluster integration test
pkg/{auth,config,logx,metrics,tlsutil,version}/   reusable libraries
web/console/               embedded web console (no build step)
deployments/               Dockerfile, compose, k8s manifests
configs/                   annotated example YAML configs
docs/                      this file, API.md, BENCHMARKS.md, HANDOFF.md
```
