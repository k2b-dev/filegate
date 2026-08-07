# Troubleshooting

Symptoms → causes → fixes, ranked roughly by frequency.

## 401 Unauthorized

- **Missing `Authorization` header.** SDK construction without a token, or a fresh `fetch` call that bypasses the SDK.
- **Wrong token format.** Must be exactly `Bearer <token>`, single space, no quotes.
- **Generated token was missed or the wrong runtime store is mounted.** An empty
  bootstrap token generates and persists one; it never enables unauthenticated
  REST. Use the once-printed value or configure an explicit token and restart.

## 404 Not Found on a path that should exist

- **External write hasn't been picked up yet.** The change detector is eventually-consistent. Either wait, or call `POST /v1/index/rescan` to force convergence.
- **Wrong mount name.** The first segment of a virtual path is the mount name (basename of the configured `base_paths` entry). `/data/foo` → mount `data`, virtual path `data/foo`.
- **Symlink pointing outside the mount.** Filegate refuses to follow these for security. The path resolves but returns 403/404 depending on where the resolution failed.

## 409 Conflict — what now?

- The body **may** have `existingId` and `existingPath`. They're populated
  for path-PUT, mkdir, upload-session create/commit, and transfer conflicts
  where the daemon could resolve the colliding node. Segment duplicate-content
  rejects and a few generic conflict paths return
  just `{"error": "conflict"}` — code defensively, fall back to a
  generic message when the diagnostic fields are missing.
- If they're present, show them to the user and let them choose
  `overwrite`, `rename`, or cancel.
- Default is `error` — explicit on purpose. If your business logic *always* wants to replace, set `onConflict: "overwrite"` consistently in your client code.
- For mkdir, if you want idempotent "make sure this folder exists", use `onConflict: "skip"`.
- For upload sessions: a 409 at commit after a clean create means another writer raced you. Ask the user whether to retry with `overwrite` or choose a new path.

Full details: [`conflict-handling.md`](conflict-handling.md).

## 413 Payload Too Large

- **Thumbnail source bigger than `ThumbnailMaxSourceBytes`** (default 64
  MiB) or decoded pixels exceed `ThumbnailMaxPixels` (default 40
  megapixels). Generate the thumbnail outside Filegate or downscale the
  source first.
- **Directory tar download exceeds preflight limits.** Directory downloads
  are capped at 100,000 tar entries, 10 GiB regular-file content, and depth
  128. Use S3/rclone or direct filesystem access for larger exports.

For invalid **upload-session** size or segment size, the daemon returns 400.
For an oversized segment body, it returns 413. For oversized **one-shot** PUT bodies, you'll typically see a connection-level
`MaxBytesError` rather than a clean HTTP status.

## 507 Insufficient Storage

The daemon's `UploadMinFreeBytes` guard refused the write. Either:

- Free up disk space on the mount.
- Lower the guard at daemon config (not recommended — the guard exists to keep the system writable for other processes).
- Move the write to a mount with more free space.

This is also returned eagerly from `POST /v1/uploads/sessions` — no segments land before the check fires.

## Browser uploads silently corrupt large files

- **You're using `JSON.stringify(body)` instead of passing the binary directly.** The TS SDK's `paths.put(path, body, opts)` expects `BodyInit` (Uint8Array, Blob, ReadableStream, etc.). Don't JSON-encode binary.
- **Missing or wrong `Content-Type`.** Pass the actual MIME type via `opts.contentType`.
- **Buffering in your relay backend.** If you do `await req.arrayBuffer()` and then forward, you've broken streaming. Use `paths.putRaw` (TS) or pass `r.Body` (Go) through directly.

## Upload session commits but downloaded file is wrong size or corrupted

- **Wrong overall checksum** in the create request. The server hashes the assembled file at commit and rejects on mismatch. Compute `sha256` over the whole file and pass `sha256:<hex>` (lowercase hex, with the prefix).
- **Wrong segment boundaries.** Use `uploads.segments.bounds(index, size, segmentSize)` from `@k2b/filegate/utils` (or the Go `segments` package) instead of manual arithmetic.
- **Trailing segment uploaded with wrong size.** The last segment is `size - (totalSegments-1) * segmentSize` bytes, not `segmentSize`. The bounds helper handles this.

## Resume loses already-uploaded segments

- **Wrong session id.** Persist the `session.id` returned by create and resume with `GET /v1/uploads/sessions/{sessionId}`. Creating a new session creates a fresh plan.
- **Staging dir was cleaned up.** The default expiry is 24h. Sessions older than that are gone. Tune `upload.expiry` in daemon config if you need longer.

## S3 multipart upload stuck in `committing`

- Startup recovery logs `recover: committing upload ... has no durable record`
  when a crash or forced shutdown happened after the manifest entered
  `committing` but before the Pebble commit.
- If the client still has the original parts list, retry
  CompleteMultipartUpload for that upload ID. Filegate leaves the staging
  directory intact so the retry can redrive the commit.
- If the client is gone and the upload is abandoned, confirm the object was not
  created, then remove the logged `.fg-uploads/s3-<uploadId>/` staging
  directory.

## "I deleted a file but it still shows up in listings"

- The change detector hasn't run yet, or ran but couldn't see the change (e.g., btrfs `find-new` race). Force a rescan: `POST /v1/index/rescan`.
- If you deleted via the API (`DELETE /v1/nodes/{id}`), this should never happen — file an issue.

## Slow listings on huge directories

- `computeRecursiveSizes=true` walks the whole subtree per child dir — O(N²) bad for huge trees. Default off; only set when you actually need recursive sizes.
- Make sure you're paginating with `pageSize` and `cursor`, not asking for everything in one call.

## Tar download is missing files

- **Symlinks are skipped on purpose** for security. If you need them, build the archive yourself.
- **A file disappeared mid-stream** — the tar walker logs and continues. The download status is still 200 because headers are already sent; check daemon logs.

## Concurrent writes to the same file

Filegate serializes its own write paths so committed mutations stay
consistent, but it does not expose a client-visible lease/lock API. If your
application has a concept of file ownership or edit sessions, enforce that in
your relay layer (for example, a per-user-per-file lock) before making
separate Filegate calls.

## Tests against Filegate are flaky

- **You're racing the change detector.** Use `POST /v1/index/rescan` to force convergence at a known point in time, instead of sleeping and hoping.
- **You're using the same mount across parallel tests.** Use a temp dir per test (and configure the daemon to mount it).
- **Filesystem mtime granularity.** ext4/tmpfs round to milliseconds. Two writes within the same millisecond can produce equal mtimes; the change detector may not see one of them. Add a tiny pause OR rewrite the test to depend on size/content rather than timing.

## I changed something in the daemon config and it's not picking up

- Bootstrap and static config needs a restart. Manifest-owned runtime keys
  activate immediately after `fg config apply`; static differences appear in
  `restartRequired` until restart.
- The TS SDK's lazy default reads env vars at first property access, which may have happened before you changed them. Use explicit construction in tests.
