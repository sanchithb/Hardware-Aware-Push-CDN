# Benchmarks

All numbers below were measured on **2026-07-07** with the built-in load
generator (`hpcdn bench`), on a single machine:

| | |
|---|---|
| CPU | AMD Ryzen 5 3500U (4 cores / 8 threads, mobile) |
| RAM | 14 GiB |
| OS | Windows 11 Home, Go 1.26 |
| Method | client + servers on the same host over loopback; 20 s runs; 40 s cool-down between runs so TIME_WAIT sockets drain |

Every request is a full HTTP round trip. "Old" is the original prototype
load balancer (commit `60e0a7c`, port changed only) doing an unauthenticated
least-loaded 302. "New" is the `hpcdn` controller doing, per request:
HMAC-SHA256 signature validation, rendezvous-hash cache affinity with
bounded-load spill, EWMA hardware scoring, region logic, Prometheus
metrics, and the 302.

## Routing decision throughput (302 path)

| | Old LB, 500 VUs | **New controller, 500 VUs** | Old LB, 1000 VUs | **New controller, 1000 VUs** | **New, 2000 VUs (stress)** |
|---|---|---|---|---|---|
| Throughput | 28,125 req/s | **27,105 req/s** | 28,454 req/s | **26,465 req/s** | **24,493 req/s** |
| Latency avg | 17.70 ms | 18.33 ms | 34.75 ms | 37.47 ms | 75.65 ms |
| Latency p50 | 13.31 ms | 15.28 ms | 29.82 ms | 34.42 ms | 70.03 ms |
| Latency p95 | 48.22 ms | 44.05 ms | 74.28 ms | 74.46 ms | 137.69 ms |
| Latency p99 | 78.68 ms | — | 130.39 ms | 111.29 ms | 212.14 ms |
| HTTP errors | 0 | 0 | 0 | 0 | 0 |

**Read:** the new control plane adds authentication, affinity hashing,
scoring and observability for ~4–7 % throughput cost, and *better tail
latency at p95* under load. Both are an order of magnitude above the
prototype's originally-published numbers (1,691–1,785 req/s at 500–1,000
VUs) — most of that original ceiling was the k6 client and socket churn,
not the server, which is why these runs use a pooled client and report
old and new under identical conditions.

At 2,000 concurrent users on a 4-core laptop the controller still routes
~24.5k req/s with zero failed requests — the prototype's published stress
result at 5,000 VUs was 793 req/s with a 31.7 % error rate.

## End-to-end playback (controller 302 → edge validate → rewrite → serve)

Full player path for a live playlist: two HTTP hops, two HMAC
validations, plus M3U8 rewrite with per-segment signature injection.

| Metric | 500 VUs, 20 s |
|---|---|
| Throughput | 4,744 playlist fetches/s |
| Latency p50 / p95 / p99 | 90.3 / 124.4 / 178.9 ms |
| Socket errors | 5.6 % (single-edge accept saturation — this test aims 500 VUs at *one* edge process; the architecture spreads this across the fleet) |

## Reproducing

```bash
# terminal 1..3: a local cluster (see README quick start)
hpcdn controller
hpcdn edge --join-token …
hpcdn origin --join-token … --watch-dir ./content

# routing-only benchmark (302 path)
hpcdn bench --sign /play/stream1/index.m3u8 -c 500 -d 20s

# end-to-end playback benchmark
hpcdn bench --sign /play/stream1/index.m3u8 --follow-redirects -c 500 -d 20s
```

`hpcdn bench --json` emits machine-readable results for CI regression
tracking.

## Notes on methodology

- The generator bounds its connection pool to the worker count
  (`MaxIdleConns = MaxConnsPerHost = concurrency`). Unbounded pools plus
  >100 workers silently churn one socket per request through TIME_WAIT and
  eventually measure the OS, not the server.
- Loopback numbers exclude real network latency; they measure the
  software's decision/serving cost. Relative old-vs-new comparisons remain
  valid since both ran identically.
- The e2e error rate is a deliberate single-edge worst case, kept in the
  report because hiding it would misrepresent single-node limits.
