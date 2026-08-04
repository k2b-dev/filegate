# HTTP Routes

Base requirements:

- API prefix: `/v1`
- Auth: `Authorization: Bearer <token>` on `/v1/*` routes, except scoped direct
  upload/download requests that use their own URL token
- Health route without auth: `GET /health`
- JSON errors: `{ "error": "..." }`. On `409 Conflict`, the body also
  carries `"existingId"` and `"existingPath"` so clients can render a
  meaningful prompt without an extra resolve call.

## CORS

CORS is disabled by default: empty `server.cors.allowed_origins` produces no
`Access-Control-*` headers and no preflight handling. Prefer configuring CORS
at Traefik/Caddy/nginx. If Filegate must answer browser cross-origin requests
directly, configure explicit origins:

```yaml
server:
  cors:
    allowed_origins:
      - "https://app.example.com"
    exposed_headers:
      - "X-Node-Id"
      - "X-Created-Id"
    max_age: "10m"
```

`allowed_methods` and `allowed_headers` are optional. When omitted, Filegate
uses REST-safe defaults for common methods plus `Authorization`,
`Content-Type`, `Filegate-Upload-Session`, and `X-Segment-Checksum`.
`allow_credentials: true` is rejected with wildcard origin `*`.

## Admin UI

The browser admin UI is a standalone SSR app in `admin/`. Filegate itself only
serves the REST API. The admin app keeps the Filegate bearer token server-side;
browser uploads use upload sessions with scoped direct session tokens.


## Conflict Handling

Endpoints that may hit a name collision accept an `onConflict` argument
with the following modes. The default is **always** `error` — Filegate
never silently overwrites or drops data.

| Mode        | Semantics                                                    | Allowed at                                       |
|-------------|--------------------------------------------------------------|--------------------------------------------------|
| `error`     | 409 Conflict if the target already exists. **Default.**       | All endpoints                                    |
| `overwrite` | Replace the existing file in place; node id preserved.       | `PUT /v1/paths`, `POST /v1/uploads/sessions`, `POST /v1/transfers` |
| `rename`    | Pick a unique sibling name (`foo.jpg` → `foo-01.jpg` → `foo-02.jpg` …) and create a new node there. The response reflects the actually-used name. | `PUT /v1/paths`, `POST /v1/nodes/{id}/mkdir`, `POST /v1/transfers`, direct one-shot uploads |
| `skip`      | If a directory with the same name exists, return it unchanged. A name conflict with a *file* still fails — we cannot turn a file into a directory. | **Only** `POST /v1/nodes/{id}/mkdir`             |

A directory can never be replaced by a file PUT (any mode), and `mkdir` can
never recursively delete an existing subtree (`overwrite` is rejected for
mkdir). For directory replacement use `POST /v1/transfers` with `overwrite`.

## Health and Stats

- `GET /health`
  - Returns `200 OK` + `OK`
- `GET /v1/stats`
  - Runtime stats for index/cache/mounts/disks and process-local system state
- `GET /v1/capabilities`
  - Runtime capability limits for adaptive clients
  - Upload fields include `maxChunkBytes`, `maxUploadBytes`,
    `maxSessionUploadBytes`, and `maxConcurrentSegmentWrites`
- `GET /v1/activity?limit=100`
  - Recent in-memory activity events for operator introspection
  - Query: `limit`, `offset`, `q`, `operation`, `outcome`
  - The actor is the authenticated Filegate credential kind (`bearer_token`,
    `s3_key`, `signed_url`, or `system`). `X-Filegate-Actor` is logged only as
    an optional delegated actor label and never participates in authorization.

## Paths API (Virtual Filesystem)

- `GET /v1/paths/`
  - List configured root nodes (virtual root entries)
- `GET /v1/paths/{path...}`
  - Return node metadata for virtual path
  - If node is a directory, response can include paged children
  - Query:
    - `pageSize` (default `100`)
    - `cursor` — opaque token from the previous page's `nextCursor`,
      passed back verbatim. Pagination survives concurrent deletion of
      the cursor entry. Legacy clients passing a bare child name keep
      working, but a deleted name then answers `400`.
    - `computeRecursiveSizes=true|false`
- `PUT /v1/paths/{path...}`
  - One-shot upload at path
  - Query: `onConflict=error|overwrite|rename` (default `error`)
  - Body: binary stream
  - Headers:
    - `X-Node-Id`: resulting node id
    - `X-Created-Id`: only when a new file was created
  - Returns `409 Conflict` (with `existingId`/`existingPath`) on collision
    when mode is `error`, or when the existing target is a directory and
    mode is `overwrite` (you cannot replace a directory via file PUT)
  - May return `507` when storage free-space guard rejects new writes

## Nodes API (ID-oriented)

- `GET /v1/nodes/{id}`
  - Metadata by id (same shape as path metadata)
  - Same directory paging query params as above
- `GET /v1/nodes/{id}/content`
  - File: raw file stream
  - Directory: tar stream of subtree
    - Preflight limit: max 100,000 tar entries, 10 GiB regular-file
      content, depth 128
    - Over-limit directory downloads return `413` before response
      headers are sent
  - Query: `inline=true|false`
- `GET /v1/nodes/{id}/thumbnail`
  - On-demand thumbnail for image-like sources
  - Query: `size`
- `POST /v1/nodes/{id}/mkdir`
  - Create subdirectory relative to parent node id
  - Body: `MkdirRequest` — `onConflict` accepts `error|skip|rename`
    (`overwrite` is rejected; use Transfer for that)
- `PUT /v1/nodes/{id}`
  - Replace file content for file node id
- `PATCH /v1/nodes/{id}`
  - Rename / ownership update
  - Query: `recursiveOwnership=true|false` (default `true`)
  - Body: `UpdateNodeRequest`
- `DELETE /v1/nodes/{id}`
  - Delete subtree

## Transfers

- `POST /v1/transfers`
  - Move or copy between parent ids
  - Query: `recursiveOwnership=true|false` (default `true`)
  - Body: `TransferRequest`

## Versions

Per-file version history for HTTP-mediated writes. btrfs-only via
reflinks; ext4 mounts return `404` with `versioning_unsupported`. See
[versioning.md](versioning.md) for behaviour, retention, and operator
notes.

- `GET /v1/nodes/{id}/versions`
  - Cursor-paginated list of captured versions (oldest first)
  - Query: `cursor=<vid>`, `limit=<n>` (default 100, max 1000)
- `GET /v1/nodes/{id}/versions/{vid}/content`
  - Streams version bytes (`application/octet-stream`)
- `POST /v1/nodes/{id}/versions/snapshot`
  - Body (optional): `{ "label"?: "..." }` (capped at `max_label_bytes`)
  - Captures current bytes immediately, ignoring cooldown. Pinned.
  - `409` if `max_pinned_per_file` would be exceeded
- `POST /v1/nodes/{id}/versions/{vid}/pin`
  - Body (optional): `{ "label"?: "..." | null }`
  - Pins version, optionally updates label. Idempotent
- `POST /v1/nodes/{id}/versions/{vid}/unpin`
  - Clears pinned flag. Label preserved. Idempotent
- `POST /v1/nodes/{id}/versions/{vid}/restore`
  - Body (optional): `{ "asNewFile"?: bool, "name"?: string }`
  - Default = in-place: snapshot current, then load target. File ID
    preserved
  - `asNewFile=true` = sibling file (`<base>-restored<ext>` or `name`,
    with `-N` conflict suffix). New File ID
- `DELETE /v1/nodes/{id}/versions/{vid}`
  - Manual purge. Works on any version (including pinned) → 204

## Search

- `GET /v1/search/glob`
  - Query:
    - `pattern` (required)
    - `paths` (comma/semicolon list)
    - `limit`
    - `showHidden`
    - `files`
    - `directories`

## Upload Sessions

Upload sessions are the resumable path for large uploads, parallel segment
delivery, and browser direct uploads. A session is explicit: create, PUT
segments in any order, then commit.

- `POST /v1/uploads/sessions`
  - Body: `UploadSessionCreateRequest`
    - `path` (required): virtual target path
    - `size` (required): final file size
    - `checksum` (required): final `sha256:<hex>`
    - `segmentSize` (optional): defaults to `upload.max_chunk_bytes`; clients
      can read `GET /v1/capabilities` to choose a compatible value
    - `contentType`, `ownership` (optional)
    - `onConflict` (optional): `error|overwrite`, default `error`
    - `direct` (optional): mint a scoped direct-session token
  - Returns `201` with session id, segment plan, uploaded segments, phase,
    and optional direct token
  - Returns `409` immediately on target conflict when `onConflict=error`
  - Returns `507` when the storage free-space guard rejects the upload
- `POST /v1/uploads/sessions:batch`
  - Body: `{ "uploads": [...], "segmentSize"?: n, "direct"?: {...} }`
  - Creates independent sessions in one request. On failure, no partial
    sessions are kept.
- `GET /v1/uploads/sessions/{sessionId}`
  - Returns current status and uploaded segment indexes
- `PUT /v1/uploads/sessions/{sessionId}/segments/{index}`
  - Body: segment bytes
  - Header: `X-Segment-Checksum: sha256:<hex>` (optional but recommended)
  - Supports out-of-order and duplicate segment uploads when content matches
  - Returns `413` when the segment body exceeds the planned segment size
- `POST /v1/uploads/sessions/{sessionId}/commit`
  - Verifies all segments and the final checksum, then atomically replaces or
    creates the target according to `onConflict`
  - Safe to retry after success; ambiguous post-replace crashes return `409`
    instead of overwriting newer data
- `DELETE /v1/uploads/sessions/{sessionId}`
  - Aborts an in-progress session and removes staged bytes

Session staging is mount-local under `.fg-uploads`, which is reserved and not
visible through normal path, node, search, or rescan APIs.

## Direct Upload URLs

Direct upload URLs let a trusted backend mint a short-lived browser upload URL
without exposing the Filegate bearer token to the browser.

- `POST /v1/uploads/direct`
  - Requires `Authorization: Bearer <token>`
  - Body:
    - `path` (required): virtual target path, for example `data/inbox/a.jpg`
    - `expiresInSeconds` (optional): default `900`, maximum `86400`
    - `contentType` (optional): required by the final uploader when set
    - `onConflict` (optional): `error|overwrite|rename`, default `error`
    - `maxBytes` (optional): per-URL body cap, must be `<= upload.max_upload_bytes`
  - Returns `201` with:
    - `uploadUrl`: absolute `PUT` URL
    - `method`: `PUT`
    - `path`
    - `expiresAt`: Unix seconds
    - `maxBytes`
- `PUT /v1/uploads/direct/{token}`
  - Does not accept the REST bearer token; the URL token is the credential
  - Body: binary stream
  - Returns the same node JSON and `X-Node-Id` / `X-Created-Id` headers as
    `PUT /v1/paths/{path...}`
  - Returns `401` for expired, malformed, or tampered URLs
  - Returns `413` when the body exceeds the URL's `maxBytes`

`server.public_url` controls the base URL used in `uploadUrl`. Leave it empty
only when the authenticated minting request already arrives with the public
host and scheme.

## Direct Download URLs

Direct download URLs let a trusted backend mint a short-lived browser download
URL without exposing the Filegate bearer token to the browser.

- `POST /v1/downloads/direct`
  - Requires `Authorization: Bearer <token>`
  - Body:
    - `nodeId` or `path` (required): exactly one resource selector
    - `expiresInSeconds` (optional): default `900`, maximum `86400`
    - `inline` (optional): sets inline content disposition when true
  - Returns `201` with:
    - `downloadUrl`: absolute `GET` URL
    - `method`: `GET`
    - `expiresAt`: Unix seconds
    - `node`: resolved node metadata
- `GET /v1/downloads/direct/{token}`
  - Does not accept the REST bearer token; the URL token is the credential
  - File: raw file stream with byte-range support
  - Directory: tar stream with the same preflight limits as
    `GET /v1/nodes/{id}/content`
  - Returns `401` for expired, malformed, or tampered URLs
  - Returns `409` when a file changed after the URL was minted
- `HEAD /v1/downloads/direct/{token}`
  - Same headers as `GET`, without a response body

File download tokens pin the resolved node id plus current file identity
(`ETag`, `sha256`, size, mtime). Directory tokens resolve the node id at mint
time and stream the current directory contents when used.

## Index Maintenance

- `POST /v1/index/rescan`
  - Force full rescan
- `POST /v1/index/resolve`
  - Resolve by path(s) or id(s)
  - Body accepts one of:
    - `path`
    - `paths[]`
    - `id`
    - `ids[]`

## Operations

These endpoints exist for operators and dashboards. All require the bearer token.

- `GET /v1/system/info`
  - Build version/commit, uptime, detector backend, effective versioning mode,
    per-mount health (`exists`, `writable`, `xattrSupported`, free/total bytes),
    and a curated set of effective limits.
  - Probes the mounts, so it touches the filesystem. Read it occasionally; do
    not poll it.
- `GET /v1/system/runtime`
  - Live counters only: detector cycles/staleness/errors (plus per-path btrfs
    generations), worker-pool queue depth and rejections, path and thumbnail
    cache hit ratios, upload sessions by phase, and segment write-slot usage.
  - Everything is read from memory, so this is the endpoint to poll.
- `GET /v1/health`
  - Dependency checks: index, detector staleness, mount reachability.
  - `200` with `status: ok|degraded`, `503` with `status: fail`.
  - Distinct from `GET /health`, which stays a plain unauthenticated `OK`
    liveness probe and checks nothing.
- `GET /v1/uploads/sessions`
  - Lists resumable upload sessions; optional `phase` filter
    (`in_progress|committing|committed|aborted`).
  - This is how an orphan left by an interrupted upload is found, since
    `DELETE /v1/uploads/sessions/{id}` needs an ID that is otherwise unknown.

Config exposure on `/v1/system/info` is an explicit allowlist of non-secret
operational values, not a dump of the config file. Anything not listed in
`LimitsInfo` stays server-side.

## Configuration

All require the bearer token. Secrets are never returned; they report
`{"configured": true|false}`.

- `GET /v1/config/schema`
  - Every key with its type, activation (`static` or `runtime`), owner
    (`manifest`, `bootstrap`, or `resource`), default and usage.
- `GET /v1/config`
  - Manifest metadata, effective and desired values, provenance, and static
    settings waiting on a restart.
- `POST /v1/config/plan`
  - Body `{"values": {"<path>": <value>}}`, containing the complete replacement
    manifest as dotted paths.
  - Validates and returns add/change/remove operations without changing state.
- `POST /v1/config/apply`
  - Body `{"values": {...}, "expectedRevision": "<revision from plan>"}`.
  - Replaces the complete managed set. Omitted keys fall back to bootstrap
    sources or defaults. `null` is rejected.
  - Returns `409` when the plan revision is stale.

## S3 access keys

Access keys are runtime resources, not configuration. Changes take effect on the
next request. Secrets are returned only by create and rotate.

- `GET /v1/s3/keys`
- `POST /v1/s3/keys` — `buckets` required; `["*"]` grants every mount
- `PATCH /v1/s3/keys/{accessKey}` — grants, rate limit, disabled flag
- `POST /v1/s3/keys/{accessKey}/rotate`
- `DELETE /v1/s3/keys/{accessKey}`

Keys configured in the static file are imported once, into a store that has
never held them, and ignored afterwards. A deleted key therefore stays deleted
across restarts even when the configuration that seeded it is still present.

## Node Shape

`Node` returns a discriminated union style via `type` (`file|directory`) with shared metadata:

- `id`, `type`, `name`, `path`, `size`, `mtime`
- `ownership { uid, gid, mode }`
- `mimeType` (when available)
- `exif` (always present; empty map when not applicable)
- directory-only optional listing fields:
  - `children[]`, `pageSize`, `nextCursor`

Source of truth for JSON structs: [`api/v1/types.go`](https://github.com/ValentinKolb/filegate/blob/main/api/v1/types.go)
