# Controller API

The full machine-readable specification is [`api/openapi.yaml`](../api/openapi.yaml)
(OpenAPI 3.0 — render it with Swagger UI, Redoc, or `npx @redocly/cli preview-docs api/openapi.yaml`).
This page is the practical guide.

## Authentication

| Surface | Auth |
|---|---|
| `/api/v1/*` admin endpoints | `Authorization: Bearer hpa_…` (printed once at first controller start; also in `<data-dir>/admin.key`) |
| `POST /api/v1/nodes/register` | join token in the body (`hpj_…`) |
| `POST /api/v1/nodes/{id}/heartbeat` | `X-HPCDN-Node-Secret` header |
| `/play/*` | HMAC query params (`hpe`, `hps`, `hpx`) |
| `/healthz`, `/metrics` | none |

Errors are always `{"error": "message"}` with a meaningful status code.

## The five requests you'll actually make

```bash
export HPCDN_API_KEY=hpa_…
CTRL=http://localhost:8080

# 1. cluster overview
curl -H "Authorization: Bearer $HPCDN_API_KEY" $CTRL/api/v1/stats

# 2. mint a join token for new nodes (24h, single use)
curl -X POST -H "Authorization: Bearer $HPCDN_API_KEY" \
  -d '{"note":"nyc pop","ttl_seconds":86400,"max_uses":1}' \
  $CTRL/api/v1/tokens

# 3. sign a playback URL (scope defaults to the stream directory, so the
#    one signature covers the playlist and every segment)
curl -X POST -H "Authorization: Bearer $HPCDN_API_KEY" \
  -d '{"path":"/play/stream1/index.m3u8","ttl_seconds":3600}' \
  $CTRL/api/v1/sign

# 4. purge a stream from every edge cache
curl -X POST -H "Authorization: Bearer $HPCDN_API_KEY" \
  -d '{"path":"stream1"}' $CTRL/api/v1/purge

# 5. drain an edge before maintenance
curl -X POST -H "Authorization: Bearer $HPCDN_API_KEY" \
  $CTRL/api/v1/nodes/nd_xxxx/drain
```

Every one of these is also a CLI command (`hpcdn status`, `hpcdn tokens
create`, `hpcdn sign`, `hpcdn purge`, `hpcdn nodes drain`) and a typed Go
call (`internal/client`).

## Signed URL format

```
/play/stream1/index.m3u8?hpe=1783499629&hps=%2Fplay%2Fstream1%2F&hpx=KiJZ6…
       │                      │               │                      │
       │                      │               │                      └ base64url HMAC-SHA256
       │                      │               └ scope: prefix the signature covers
       │                      └ expiry (unix seconds, UTC)
       └ object path
```

Signature = `HMAC-SHA256(signing_key, scope + "\n" + expiry)`; when no
scope is present the path itself is signed. Edges validate locally — no
controller round trip on the media path. Comparison is constant-time.

## Live events

`GET /api/v1/events` is an SSE stream (`event:` = type, `data:` = JSON).
Types: `node_joined`, `node_recovered`, `node_ejected`, `node_drain`,
`node_removed`, `purge`, `settings`. Recent events are replayed on
connect. Note `EventSource` cannot send an `Authorization` header — use
`fetch` with a streaming reader (the embedded console does exactly this).

## Metrics

`GET /metrics` on every role, Prometheus text format. Highlights:

| Metric | Where | Meaning |
|---|---|---|
| `hpcdn_route_decisions_total` / `hpcdn_route_errors_total` | controller | routing volume / 503s |
| `hpcdn_route_duration_seconds` (histogram) | controller | decision latency |
| `hpcdn_auth_failures_total` | controller | rejected keys/signatures |
| `hpcdn_edge_play_requests_total`, `hpcdn_edge_play_duration_seconds` | edge | data-plane traffic |
| `hpcdn_edge_origin_pulls_total` | edge | cache-miss fills |
| `hpcdn_origin_pushes_total`, `hpcdn_origin_push_failures_total`, `hpcdn_origin_queue_depth` | origin | distribution pipeline health |
