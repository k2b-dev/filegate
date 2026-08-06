# Authentication & Error Model

## Authentication

The REST API has one bearer token, resolved at daemon startup. If bootstrap
configuration leaves it empty, Filegate generates a strong token, stores it in
`storage.runtime_config_path`, and prints it once. Empty never enables
unauthenticated REST, including in S3 deployments.

```http
Authorization: Bearer <token>
```

Required on every `/v1/*` request except scoped direct upload/download URLs.
Missing or wrong → `401 Unauthorized`.

Auth-free endpoints:

- `GET /health` (for k8s liveness probes and similar)
- `PUT /v1/uploads/direct/{token}` after a trusted backend minted the signed URL
- `GET /v1/downloads/direct/{token}` / `HEAD /v1/downloads/direct/{token}` after a trusted backend minted the signed URL

### What this means

- **No per-user auth.** Filegate is infrastructure. Per-user authorization belongs in your backend, which fronts Filegate. See [`relay-patterns.md`](relay-patterns.md).
- **No scopes / OAuth / JWT.** A token either has full access or none.
- **Token rotation requires a restart and usually a maintenance window** — the
  token is static and Filegate accepts only one at a time. There is no native overlap mechanism. To
  rotate without downtime you'd need an external auth proxy in front of
  the daemon that translates between an old and new token; that's outside
  Filegate's scope.

### Token sourcing

| Where you read the token from | Use case                                              |
|-------------------------------|-------------------------------------------------------|
| Env var `FILEGATE_TOKEN`      | Default for the TS SDK's lazy server instance         |
| Secret manager (Vault, etc.)  | Production backend                                    |

**Never** embed the Filegate token in browser code or a public repo.
The bearer token has no scopes or per-user auth — leaking it is a full
compromise. Browsers should use your backend relay, or direct upload/download
URLs that your backend minted for one resource and expiry (see
[`relay-patterns.md`](relay-patterns.md)).

## Error model

Most non-2xx responses carry a JSON body shaped like:

```json
{
  "error": "human-readable message"
}
```

Exceptions: `304 Not Modified` (returned by thumbnail endpoint on cache
hit) has no body. `204 No Content` (DELETE success) also has no body.
Always check `Content-Length`/`Content-Type` before attempting to decode.

For `409 Conflict`, the body is enriched with diagnostic fields:

```json
{
  "error": "filename already exists in parent",
  "existingId": "01933abc-...",
  "existingPath": "data/users/alice/photo.jpg"
}
```

Use these to render meaningful prompts ("File X already exists, would you like to overwrite or rename?") without an extra resolve call.

## Status code map

| Code | Meaning                                                                    | Typical cause                                                |
|------|----------------------------------------------------------------------------|--------------------------------------------------------------|
| 200  | OK                                                                         | Resource read; existing resource updated; status response     |
| 201  | Created                                                                    | New file/directory created                                    |
| 204  | No Content                                                                 | Successful DELETE                                             |
| 304  | Not Modified                                                               | Thumbnail cache hit (conditional request matched ETag)        |
| 400  | Bad Request                                                                | Malformed JSON, invalid path, unknown `onConflict` mode       |
| 401  | Unauthorized                                                               | Missing or wrong bearer token                                 |
| 403  | Forbidden                                                                  | Path traversal attempt, symlink escape, root mutation         |
| 404  | Not Found                                                                  | Node ID doesn't exist; path doesn't resolve                   |
| 409  | Conflict                                                                   | Name already exists with `onConflict: error`; cross-type collision |
| 413  | Payload Too Large                                                          | Image source too large, segment body too large, or capped direct upload exceeded |
| 415  | Unsupported Media Type                                                     | Thumbnail of an unsupported image format                      |
| 503  | Service Unavailable                                                        | Thumbnail scheduler queue is full                             |
| 500  | Internal Server Error                                                      | Unexpected daemon failure (look at daemon logs)               |
| 507  | Insufficient Storage                                                       | Free-space guard (`UploadMinFreeBytes`) would be violated     |

For upload sessions, the daemon validates `size` and `segmentSize` at
create time and rejects invalid requests with **400 Bad Request**. A segment
body larger than the planned segment size returns **413 Payload Too Large**.

## SDK error handling

### TS

```ts
import { FilegateError } from "@valentinkolb/filegate/client";

try {
  await fg.paths.put(path, body);
} catch (e) {
  if (e instanceof FilegateError) {
    console.log(e.status, e.message, e.method, e.path);
    // The raw response body string is on `e.body`.
    // The parsed envelope (when valid JSON in the documented shape) is on
    // `e.errorResponse` — use that for typed conflict diagnostics:
    if (e.status === 409 && e.errorResponse) {
      console.log(e.errorResponse.existingId, e.errorResponse.existingPath);
    }
  } else {
    // network failure, fetch-level error, etc.
  }
}
```

### Go

```go
_, err := fg.Paths.Put(ctx, path, body, filegate.PutPathOptions{})
if err != nil {
    var apiErr *filegate.APIError
    if errors.As(err, &apiErr) {
        log.Printf("filegate → %d: %s", apiErr.StatusCode, apiErr.Message)
        if apiErr.IsConflict() {
            // Typed diagnostic fields, populated by the server on 409:
            log.Printf("collides with %s (id %s)", apiErr.ExistingPath, apiErr.ExistingID)
        }
    } else {
        // ctx canceled, network error, etc.
    }
}
```

`*APIError` is only returned by the **non-`Raw`** SDK methods. The `*Raw`
methods (`PathsClient.PutRaw`, `NodesClient.ContentRaw`, etc.) return the
raw `*http.Response` on 4xx/5xx so relay handlers can forward the upstream
status, headers, and body unchanged. See [`relay-patterns.md`](relay-patterns.md).

## Logging in your relay backend

Recommended fields when logging Filegate calls:

- `method` and `path` (so you can correlate with daemon access logs)
- `status` (the HTTP status code from Filegate)
- `latency_ms` (time spent waiting on Filegate)
- Optionally: `upload_bytes`, `node_id`, `user_id`

This makes it trivial to find "user X tried to upload Y at time Z and got status N" — which is the only way to debug user-reported issues.

## Health & readiness probes

For Kubernetes, keep the unauthenticated liveness probe on `/health`. The deep
readiness route requires the bearer token, so inject its header with your
platform's secret mechanism or check it through a trusted sidecar/proxy:

```yaml
livenessProbe:
  httpGet:
    path: /health
    port: 8080
  initialDelaySeconds: 5
  periodSeconds: 10

readinessProbe:
  httpGet:
    path: /v1/health
    port: 8080
    httpHeaders:
      - name: Authorization
        value: Bearer <injected-secret>
  initialDelaySeconds: 5
  periodSeconds: 5
```

`/health` doesn't require auth and doesn't exercise dependencies, so it is only
liveness. `/v1/health` checks the index, detector staleness, and mount
reachability; `fail` returns 503. Read `/v1/system/info` after start for slower
writability, xattr, reflink, and effective-mode checks. `/v1/stats` is not a
readiness endpoint.
