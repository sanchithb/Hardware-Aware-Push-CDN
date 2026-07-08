# HANDOFF — continuation guide for an AI (or human) picking this up

This document exists so that any capable engineer or AI system can resume
work with zero conversation context. Read this, then `ARCHITECTURE.md`,
then the code. Last updated: **2026-07-07**, after the full v1 rebuild.

## 1. What this project is

`hpcdn` — a self-hosted, hardware-aware push CDN in Go. One binary, three
roles (controller / edge / origin), plus an admin CLI, an embedded web
console, a REST API (OpenAPI spec in `api/openapi.yaml`), Prometheus
metrics, tests, benchmarks and deployment manifests.

**History that matters:** the repo previously contained a ~370-line
prototype (three standalone `main.go` files) and a README describing
features that did not exist — every file under the old `internal/`, `pkg/`,
`deployments/` was 0 bytes. The 2026-07 rebuild (this codebase) implemented
the system for real. The old prototype is still reachable at git commit
`60e0a7c` (`git show 60e0a7c:cmd/loadbalancer/main.go`) and is used as the
benchmark baseline in `docs/BENCHMARKS.md`.

## 2. State: what is DONE and VERIFIED

Everything below was built, compiled, `go vet`-clean, and verified — unit
tests, an in-process e2e cluster test (`internal/e2e`), and a manual live
cluster (controller + 2 edges + origin) on Windows:

- Controller: enrollment (hashed join tokens, TTL/max-use), heartbeats,
  EWMA scoring, telemetry history, directive queue (seq/ack), active
  health prober with circuit breaker + panic threshold, routing
  (rendezvous affinity + bounded loads + region penalty), REST API, SSE,
  JSON persistence (atomic), Prometheus metrics, embedded console.
- Edge: SIEVE disk cache (checksum-verified atomic writes, restart
  rebuild, purge), signed playback with local HMAC validation, HLS header
  discipline, byte ranges, CORS, hold-and-serve + singleflight
  pull-through, playlist rewriting (incl. `URI="…"` attributes), drain.
- Origin: debounced recursive watcher, concurrent push with SHA-256 +
  retries, `/fetch` pull-through endpoint, topology from heartbeats.
- CLI: role runners + status/nodes/tokens/sign/purge/settings/logs/bench.
- Console (`web/console`, no build step): overview, nodes, node detail,
  enroll, settings, events/logs; login gate; light/dark; SSE-over-fetch;
  hand-rolled SVG charts. Screenshot-verified with live data.
- Verified end-to-end by hand: push → 302 route → byte-identical
  playback, 206 ranges, 403 on tamper/expiry/unsigned, purge →
  pull-through refill, drain → traffic steers away → undrain → affinity
  returns, stream affinity pins to one edge.
- Benchmarked old vs new (numbers + method in `docs/BENCHMARKS.md`).

Test suite: `go test ./...` — all green (8 test packages, includes the
e2e cluster test; ~15 s total). Use `-short` to skip e2e.

## 3. Invariants — do not break these

1. **Credentials at rest are hashed** (admin key, join tokens, node
   secrets) — `store.Hash` (SHA-256). Only the cluster signing/ingest
   keys are stored raw because nodes need them. Never log plaintext
   credentials; they are shown exactly once at creation.
2. **Signature validation is constant-time** (`hmac.Equal`,
   `subtle.ConstantTimeCompare`). Keep it that way in new paths.
3. **The controller never needs an inbound connection to nodes for
   control** — directives ride heartbeat responses (seq/ack,
   at-least-once, idempotent handlers). The one exception is the health
   prober (`GET /healthz`), which only powers circuit breaking.
4. **Cache writes are atomic**: temp file + rename, checksum before
   visibility. A partially-written object must never be servable.
5. **`internal/protocol` is the single source of wire truth.** Both sides
   import it. Change requests/responses there or nowhere.
6. **The console is dependency-free** (no npm, no build step, embedded via
   `go:embed`). Untrusted strings enter the DOM via `textContent` only.
7. **Config precedence**: defaults < YAML < `HPCDN_*` env < flags
   (implemented by snapshot/restore of changed flags in
   `internal/cli/run.go` — YAML load would otherwise clobber flags).
8. Keys/prefixes (`hpa_`/`hpj_`/`hpn_`/`hps_`/`hpi_`) and signed-URL param
   names (`hpe`/`hps`/`hpx`) are wire-visible contracts documented in the
   OpenAPI spec — changing them breaks deployed nodes and minted URLs.

## 4. Known gotchas (cost real debugging time)

- **Windows + Git Bash mangles URL paths**: `/play/...` becomes
  `C:/Program Files/Git/play/...`. Set `MSYS_NO_PATHCONV=1` when testing
  with curl; also `-o /dev/null` breaks under it (curl exit 23) — write to
  a real file.
- **Windows file-notification batching**: write events can arrive in
  bursts separated by more than the debounce window, so one file can
  legitimately emit two pushes. Harmless (pushes are idempotent PUTs).
  The watcher's timer race (Reset on a fired timer double-emitting) is
  fixed via a timer-identity check in `watcher.go` — don't simplify it away.
- **Load generator socket churn**: Go's default `MaxIdleConns=100` +
  concurrency>100 churns one socket per request → TIME_WAIT exhaustion →
  (on Windows) blocked dials → thread explosion. `internal/bench` bounds
  the pool to the worker count. Any new HTTP fan-out code should think
  about this.
- **`http.Server.ServeTLS` with empty cert paths** works only when
  `TLSConfig` carries certificates (self-signed path). Both branches
  exist in each role's `Run()`.
- **Windows can't rename over an open file** — `cache.Put` retries after
  removing the destination. Keep that fallback.

## 5. How to build / run / verify

```bash
make build          # or: go build -o bin/hpcdn ./cmd/hpcdn
make test           # full suite incl. e2e (all green as of handoff)
go vet ./...        # clean as of handoff

# local cluster smoke test (3 terminals or background jobs):
bin/hpcdn controller                         # prints hpa_… admin key once
export HPCDN_API_KEY=hpa_…
bin/hpcdn tokens create --note dev           # → hpj_…
bin/hpcdn edge   --join-token hpj_… --listen :8081
bin/hpcdn origin --join-token hpj_… --watch-dir ./content
# drop .ts/.m3u8 files into ./content/<stream>/, then:
bin/hpcdn sign /play/<stream>/index.m3u8
# console: http://localhost:8080
```

## 6. Where things live (fastest map)

| Concern | File |
|---|---|
| Routing algorithm | `internal/controller/router/router.go` |
| Scoring / heartbeat ingest / prober | `internal/controller/registry/registry.go` |
| REST handlers | `internal/controller/api/api.go` |
| SIEVE cache | `internal/edge/cache/cache.go` |
| Viewer data path (hold/pull/rewrite) | `internal/edge/serve/serve.go` |
| Push pipeline | `internal/origin/pusher/pusher.go` |
| Wire types | `internal/protocol/protocol.go` |
| Signed URLs / tokens | `pkg/auth/auth.go` |
| CLI commands | `internal/cli/*.go` |
| Console SPA | `web/console/{index.html,app.js,style.css}` |

## 7. Roadmap — prioritized, with design notes

Nothing below is started. Ordered by value-per-effort:

1. **Controller HA** (the known single point of failure; redirect mode
   only degrades new sessions, existing streams keep playing off edges).
   Simplest honest path: active/standby with the JSON store replicated
   (raft via `hashicorp/raft`, or lease-based failover over shared
   storage). Nodes already retry heartbeats forever, so failover is
   mostly a client-list problem.
2. **LL-HLS support** in the edge: blocking playlist reload
   (`_HLS_msn`/`_HLS_part` query params — hold the playlist request until
   that media sequence exists; the hold-and-serve machinery in
   `serve.go` is 80 % of it), `EXT-X-PRELOAD-HINT` handling.
3. **Tiered caching / shield PoPs**: let an edge declare a parent edge
   instead of pulling straight from origin (one extra hop in
   `pullFromOrigin`, topology field in `HeartbeatResponse`). This is what
   cuts origin egress at real scale.
4. **Byte-range–aware partial caching** for large VOD files (currently
   whole-object caching; ranges are served from complete objects only).
5. **Console: cluster-level time series** — persist aggregate stats
   server-side (ring buffer, same pattern as per-node history) so the
   overview chart survives page reloads.
6. **`hpcdn bench` distributed mode** — many workers, one aggregator, to
   benchmark beyond a single client machine's socket ceiling.
7. **Access logs + OpenTelemetry traces** (research says OTel correlation
   edge→origin is now table stakes for CDN buyers).
8. **Signed cookies** as an alternative to query params (CloudFront-style,
   for players that strip query strings).
9. **Config reload on SIGHUP** and `hpcdn validate` for configs.
10. **Multi-origin content namespaces** — today all origins serve one
    keyspace; add per-origin path prefixes in the registry.

## 8. Release checklist (when cutting v1.0.0)

- `make release` cross-compiles; ldflags stamp version/commit/date.
- Update README benchmark table if re-run on different hardware.
- Docker image: `make docker`; compose file expects image context `..`.
- Tag, then verify `hpcdn version` output in each artifact.
