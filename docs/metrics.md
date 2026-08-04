# Prometheus Metrics

Filegate exposes Prometheus metrics on the **existing REST listener** (no
extra port) at a configurable path. The endpoint is **off by default** —
enable it in config.

```yaml
metrics:
  enabled: true          # default false
  path: "/metrics"       # served on the REST listener (server.listen)
  token: ""              # layered auth — see below
```

## Layered authentication

The `/metrics` endpoint auth follows a layered rule so you rarely need a
dedicated credential, but can have one:

1. If `metrics.token` is set → that token is required (`Authorization: Bearer <token>`).
2. Else if `auth.bearer_token` (the REST token) is set → that token is required.
3. Else → the endpoint is served **openly** (no auth).

The open case is intentional: on a trusted internal network where the
Prometheus scraper holds no filegate credentials, leave both empty and
rely on network isolation. Use `metrics.token` when you want the scraper
to authenticate with a credential distinct from the REST API token (e.g.
a read-only scrape secret you can rotate independently).

> **Reaching the open mode:** `auth.bearer_token` is normally required at
> startup. It may be left empty **only when `s3.enabled=true`** — an
> S3-only deployment authenticates via SigV4 and runs with the REST API
> locked down (every `/v1` route returns 401). That is the configuration
> in which both tokens can be blank and `/metrics` is served openly. A
> REST-enabled daemon always has a bearer token, so `/metrics` falls back
> to it rather than serving openly.

`metrics.path` must not collide with the REST surface — `/health` and
anything under `/v1` are rejected at startup.

## Environment overrides

All three knobs bind via env (mirrors the rest of the config):

```
FILEGATE_METRICS_ENABLED=true
FILEGATE_METRICS_PATH=/internal/metrics
FILEGATE_METRICS_TOKEN=scrape-secret
```

## Scrape config

```yaml
# prometheus.yml
scrape_configs:
  - job_name: filegate
    metrics_path: /metrics
    static_configs:
      - targets: ["filegate:8080"]
    authorization:
      type: Bearer
      credentials: "<metrics.token or auth.bearer_token>"
```

Drop the `authorization` block when the endpoint is open.

---

## Metric reference

### HTTP (RED method)

Recorded by a middleware wrapping both the REST and S3 adapters. The
`adapter` label is `rest` or `s3`; `op` is the S3 operation (PutObject,
GetObject, CompleteMultipartUpload, …) or the HTTP method for the REST
adapter; `status_class` is `2xx`/`3xx`/`4xx`/`5xx`.

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `filegate_http_requests_total` | counter | adapter, op, status_class | Total requests. |
| `filegate_http_request_duration_seconds` | histogram | adapter, op | Request latency. |
| `filegate_http_requests_in_flight` | gauge | adapter | Concurrent in-flight requests. |
| `filegate_http_request_size_bytes` | histogram | adapter, op | Request body size (from Content-Length). |
| `filegate_http_response_size_bytes` | histogram | adapter, op | Response body size. |

> **Note on the `op` label:** requests rejected *before* dispatch
> (authentication / authorization failures, rate-limit 503s) carry the
> coarser HTTP method as `op` rather than the specific operation name,
> because the operation isn't classified until after the auth gate.

### Saturation / domain gauges

Read at scrape time (an `svc.Stats()` plus a `statfs` per mount).

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `filegate_mount_free_bytes` | gauge | mount | **Free bytes on the mount's filesystem — alert on this.** |
| `filegate_mount_used_bytes` | gauge | mount | Used bytes on the mount's filesystem. |
| `filegate_index_entities` | gauge | type=files\|dirs | Indexed entity counts. |
| `filegate_index_db_bytes` | gauge | — | On-disk Pebble index size. |
| `filegate_path_cache_entries` | gauge | — | Path-resolution cache entries. |

### Background loops + rate limiting

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `filegate_multipart_cleanup_retired_total` | counter | reason=done\|aborted\|stuck | Multipart staging dirs retired. |
| `filegate_multipart_cleanup_errors_total` | counter | — | Cleanup-loop errors. |
| `filegate_version_prune_deleted_total` | counter | — | Versions pruned. |
| `filegate_version_prune_kept_total` | counter | — | Versions kept by retention. |
| `filegate_version_prune_errors_total` | counter | — | Pruner errors. |
| `filegate_detector_events_total` | counter | type=created\|changed\|deleted\|unknown | Filesystem-detector events. |
| `filegate_s3_ratelimit_rejected_total` | counter | — | S3 requests rejected with 503 SlowDown. |

### Hot-path latency (the trace substitute)

The multipart Complete path is the most complex multi-step operation, so
its sub-phases are timed individually. A slow Complete is almost always
one of: lock contention, the whole-body re-hash, or the Pebble commit —
this histogram tells you which without distributed tracing.

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `filegate_multipart_complete_phase_seconds` | histogram | phase=concat\|lock_wait\|hash\|pebble_batch | Per-phase Complete duration. |

### Detector and cache

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `filegate_detector_stale_seconds` | gauge | — | Seconds since the detector last completed a scan round. Growing far past the scan interval means detection stopped and the index is silently drifting from the filesystem. |
| `filegate_detector_cycles_total` | counter | — | Detection scan rounds completed. |
| `filegate_detector_errors_total` | counter | — | Detection scan errors. |
| `filegate_path_cache_lookups_total` | counter | result=hit\|miss | Path cache lookups by result. Occupancy alone cannot tell an undersized cache from a cold one. |

Worker-pool saturation is deliberately not a metric: the scheduler lives inside
the HTTP router, which the metrics provider cannot reach without an awkward
back-channel. It is available on `GET /v1/system/runtime` instead.

### Runtime + process (free, from client_golang)

Standard Go-runtime and process collectors are registered:
`go_goroutines`, `go_gc_duration_seconds`, `go_memstats_*`,
`process_cpu_seconds_total`, `process_resident_memory_bytes`, and —
critically for a file gateway — `process_open_fds` (file-descriptor leak
detection).

---

## Example PromQL

```promql
# 5xx error rate (per adapter)
sum by (adapter) (rate(filegate_http_requests_total{status_class="5xx"}[5m]))
  / sum by (adapter) (rate(filegate_http_requests_total[5m]))

# p95 request latency by op (S3)
histogram_quantile(0.95,
  sum by (le, op) (rate(filegate_http_request_duration_seconds_bucket{adapter="s3"}[5m])))

# Disk-fill alert: less than 10% free on any mount
filegate_mount_free_bytes
  / (filegate_mount_free_bytes + filegate_mount_used_bytes) < 0.10

# File-descriptor growth (leak smell)
deriv(process_open_fds[15m]) > 0

# Goroutine trend
filegate_http_requests_in_flight

# Multipart Complete — which phase dominates p95?
histogram_quantile(0.95,
  sum by (le, phase) (rate(filegate_multipart_complete_phase_seconds_bucket[5m])))

# Rate-limit pressure
rate(filegate_s3_ratelimit_rejected_total[5m])
```

Grafana dashboards are the operator's responsibility — filegate exposes
the metrics; you visualize them.

---

## Cardinality discipline

Labels are deliberately bounded: `status_class` (not the exact code),
`adapter` (two values), `op` (a fixed set), `mount`, `reason`, `phase`,
`type`. There are **no** per-path, per-key, or per-access-key labels —
those are unbounded and would inflate the time-series database.

## Deliberately not exposed (yet)

- **Multipart uploads-in-flight gauge** — would require scanning
  `.fg-uploads/` on every scrape (I/O on the scrape path). Deferred until
  it can be tracked in memory cheaply.
- **Per-access-key rate-limit label** — cardinality risk with many keys.
  The aggregate `filegate_s3_ratelimit_rejected_total` is exposed; a
  per-key breakdown can be added behind a flag if needed.
