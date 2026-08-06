# Filegate — Function Overview

This is the canonical "what can Filegate do?" reference. Read top to bottom the first time you integrate; come back to specific sections as needed.

## Table of contents

1. [Mental model](#mental-model)
2. [Mounts](#mounts)
3. [Stable file IDs](#stable-file-ids)
4. [Browse: paths and nodes](#browse-paths-and-nodes)
5. [Upload: one-shot](#upload-one-shot)
6. [Upload: sessions / resumable](#upload-sessions--resumable)
7. [Download](#download)
8. [Mkdir](#mkdir)
9. [Update: rename, ownership](#update-rename-ownership)
10. [Delete](#delete)
11. [Transfer: move / copy](#transfer-move--copy)
12. [Search (glob)](#search-glob)
13. [Thumbnails](#thumbnails)
14. [EXIF metadata](#exif-metadata)
15. [Per-file versioning](#per-file-versioning)
16. [Index maintenance](#index-maintenance)
17. [Stats and observability](#stats-and-observability)
18. [Conflict handling](#conflict-handling)
19. [Authentication](#authentication)
20. [Free-space safety](#free-space-safety)
21. [What Filegate does NOT do](#what-filegate-does-not-do)

---

## Mental model

```
                         ┌──────────────────────┐
                         │  Filegate daemon     │ /v1/*  (Bearer auth)
   client app  ──HTTP──▶ │  (Linux only)        │
                         │                      │
                         │  ┌────────────────┐  │
                         │  │ Pebble index   │  │ ← path, id, metadata
                         │  └────────────────┘  │
                         │  ┌────────────────┐  │
                         │  │ Linux FS       │  │ ← actual file content
                         │  │  /data (mount) │  │
                         │  └────────────────┘  │
                         └──────────────────────┘
```

You give Filegate one or more **mount roots** (e.g. `/data`). Filegate exposes them as **virtual mounts** with friendly names. Inside each mount you have a normal directory tree of files and folders. Filegate maintains a metadata index over the tree, so listing/lookup is O(1)-ish even for huge directories.

External writes to the mount (someone else's process, rsync, btrfs receive) are picked up automatically by the change detector.

## Mounts

A mount is a top-level virtual root. Configured at daemon startup via `storage.base_paths`. Each mount has:

- **Name** — derived from the basename of the mount path (`/data` → `data`).
- **ID** — a stable Filegate file ID for the mount root.
- **Path** — the absolute filesystem path on the daemon host.

```http
GET /v1/paths/
→ { "items": [{ "id": "...", "name": "data", "type": "directory", ... }] }
```

You'll typically have 1–N mounts. From the client's perspective, a virtual path is `<mount-name>/<rest>` (e.g. `data/users/alice/photos/sunset.jpg`).

## Stable file IDs

Every file and directory under a mount gets a 16-byte UUID v7 the first time Filegate sees it. The ID is stored as the Linux xattr `user.filegate.id` directly on the file, so it survives:

- Renames (`mv old new`)
- Moves to other directories
- `cp -a` if `--preserve=xattrs` is used
- Daemon restarts (re-reads xattr on rescan)

It does NOT survive:

- A copy that doesn't preserve xattrs
- An rsync without `-X` / `--xattrs`
- Manual `setfattr -x user.filegate.id <file>`

**Practical impact:** store IDs in your own DB, not paths. To address a node by ID:

```http
GET    /v1/nodes/{id}            ← metadata
GET    /v1/nodes/{id}/content    ← raw bytes (or tar for directories)
PUT    /v1/nodes/{id}            ← replace content of existing file
PATCH  /v1/nodes/{id}            ← rename / change ownership
DELETE /v1/nodes/{id}            ← remove subtree
```

To convert between paths and IDs:

```http
POST /v1/index/resolve   { "path": "data/x/y" }     → { "item": { "id": "...", ... } }
POST /v1/index/resolve   { "id": "01933..." }       → { "item": { "path": "data/x/y", ... } }
POST /v1/index/resolve   { "paths": [...] }         → batch
POST /v1/index/resolve   { "ids":   [...] }         → batch
```

## Browse: paths and nodes

```http
GET /v1/paths/                              ← list all mounts (root entries)
GET /v1/paths/{path...}                     ← metadata for a virtual path
GET /v1/nodes/{id}                          ← metadata for a node by ID
```

For directory nodes the response can include children:

```
GET /v1/nodes/{dirID}?pageSize=100&cursor=&computeRecursiveSizes=false
→ {
    "id": "...", "type": "directory", "name": "...",
    "children": [...],      ← present for directories
    "pageSize": 100,
    "nextCursor": "..."     ← empty on last page
  }
```

Cursor pagination is by name within the directory. Directories are listed before files (kind-byte sort in the index).

`computeRecursiveSizes=true` walks the subtree to fill in `size` for each child directory — expensive on large trees, off by default.

## Upload: one-shot

For files that fit comfortably in memory or a single HTTP body:

```http
PUT /v1/paths/data/uploads/hello.txt?onConflict=error
Content-Type: text/plain
Body: <binary stream>

→ 201 Created (new file) or 200 OK (overwrite)
   X-Node-Id: 01933abc...
   X-Created-Id: 01933abc...   ← only present when newly created
   { ...full Node metadata... }
```

Intermediate directories (`data/uploads/`) are created automatically (`mkdir -p` style, always reuse-if-dir).

**Limits:** Daemon-configured `MaxUploadBytes` (default 500 MiB). Larger files should use upload sessions.

**Conflict handling:** `?onConflict=error|overwrite|rename`, default `error`. See [conflict-handling.md](conflict-handling.md).

## Upload: sessions / resumable

For large files, parallel segment uploads, resumable transfers across network
interruptions, and out-of-order delivery:

```
1. POST /v1/uploads/sessions                     → returns session id + segment plan
2. PUT  /v1/uploads/sessions/{id}/segments/{n}   ← repeat for each segment
3. POST /v1/uploads/sessions/{id}/commit         → verify checksum + publish atomically
```

Key properties:

- **Durable session ID**: persist `session.id` and resume with status.
- **Out-of-order**: segments can arrive in any order.
- **Duplicate-safe**: re-uploading the same segment with the same content is a no-op.
- **Explicit commit**: the server verifies the assembled checksum before publishing.
- **Conflict-aware**: `onConflict` is checked at create (optimistic, saves bandwidth) and at commit (race-safe).

See [upload-sessions.md](upload-sessions.md) for the full state machine and resume patterns.

## Download

By ID:

```http
GET /v1/nodes/{id}/content?inline=false
```

- For a **file node**: streams the raw bytes. `Content-Type` is derived
  from the file extension at response time (usually matches `meta.mimeType`).
  `Content-Disposition` is `attachment` by default; pass `?inline=true` to
  render in-browser.
- For a **directory node**: streams a `application/x-tar` archive of the
  subtree. Symlinks are skipped (security). `Content-Disposition` is
  `attachment; filename="<name>.tar"`. Directory tar preflight rejects
  over 100,000 tar entries, over 10 GiB regular-file content, or depth over
  128 with `413`.

The download path is streaming — large files don't materialize in memory.

## Mkdir

Create a directory under a parent node:

```http
POST /v1/nodes/{parentID}/mkdir
{
  "path": "subdir/with/intermediates",
  "recursive": true,
  "ownership": { "uid": 1000, "gid": 1000, "mode": "750", "dirMode": "750" },
  "onConflict": "error"     ← default
}
```

- `recursive: true` (default) creates intermediate dirs. With `recursive: false`, all parents must exist.
- Intermediate segments are always **reused-if-dir** regardless of `onConflict`. Only the leaf segment respects the mode.
- Allowed modes: `error | skip | rename`. **`overwrite` is rejected** for mkdir — replacing a directory subtree is a Transfer operation.
- `skip` mode is the idempotent-folder pattern (mkdir -p): existing directory is returned unchanged.

## Update: rename, ownership

```http
PATCH /v1/nodes/{id}?recursiveOwnership=true
{
  "name": "newname.txt",                       ← rename within same parent
  "ownership": { "uid": 1000, "gid": 1000, "mode": "640", "dirMode": "750" }
}
```

- `name` triggers a rename in place. Cannot contain `/`.
- `ownership` applies UID/GID/mode. For directories, `recursiveOwnership=true` (default) descends.

## Delete

```http
DELETE /v1/nodes/{id}
→ 204 No Content
```

Removes the node and its entire subtree from disk and the index. There is no soft-delete or trash — gone is gone.

## Transfer: move / copy

For relocating or duplicating subtrees:

```http
POST /v1/transfers?recursiveOwnership=true
{
  "op": "move",                                ← or "copy"
  "sourceId": "01933...",
  "targetParentId": "01933...",
  "targetName": "destination-name",
  "onConflict": "error",                       ← default; or overwrite/rename
  "ownership": { ... }
}
```

- `move` rewrites the index entries; the file ID is preserved across the move.
- `copy` copies bytes (xattrs preserved when supported); the copied node gets fresh IDs.
- For "try the name, append `-NN` if taken" semantics, use `onConflict: "rename"`. There is no separate `ensureUniqueName` field — sending unknown JSON fields returns 400.

## Search (glob)

```http
GET /v1/search/glob?pattern=**/*.jpg&paths=data,backups&limit=200&showHidden=false&files=true&directories=false
```

- `pattern` uses doublestar globbing (`**` matches any depth).
- `paths` is a comma- or semicolon-separated list of virtual paths. Each
  entry can be a mount name (the common case, e.g. `data`) or any virtual
  directory path under a mount (e.g. `data/users/alice/photos`). Empty
  list = all mounts.
- `limit` is a fairness cap split across the listed paths.
- Returns `{ results: [Node...], errors: [...], paths: [...], meta: {...} }`.

Glob matching uses index entries — does not walk the filesystem on each call. Fast.

## Thumbnails

```http
GET /v1/nodes/{imageID}/thumbnail?size=256
→ image/jpeg
```

- Allowed `size` values: **128, 256, 512** (default 256). Other values are
  rejected with 400.
- Generated on-demand from the source file using `disintegration/imaging` +
  EXIF orientation.
- Cached in an in-process LRU (configurable size).
- Sends `ETag` and `Last-Modified` so browsers cache aggressively. A
  conditional GET that matches returns **304 Not Modified**.
- Limits: source file size (default 64 MiB), decoded pixel count (default
  40 megapixels). Oversized sources return 413.
- Unsupported source formats return **415 Unsupported Media Type**.
- Job queue is bounded; if it's full the response is **503 Service Unavailable**.

Supported source formats are determined by the image decoders that the
daemon registers at startup. As of the current build that's **JPEG, PNG,
GIF, and WebP**. Other formats return 415 Unsupported Media Type.

## EXIF metadata

EXIF is extracted synchronously when reading file metadata for image-like sources (JPEG, TIFF, DNG). Returned as `meta.exif: { "Make": "...", "Model": "...", ... }` (always present in the response, empty map when not applicable).

EXIF data is also persisted in the index, so subsequent reads don't re-parse.

## Per-file versioning

```http
GET  /v1/nodes/{fileID}/versions
POST /v1/nodes/{fileID}/versions/snapshot
POST /v1/nodes/{fileID}/versions/{versionID}/restore
```

REST and S3 overwrite paths can capture per-file versions. Older
versions are listable, downloadable, pinnable, restorable in place, or
restorable as a fresh sibling file.

- The management surface is REST-only; S3 does not expose S3 object versions.
- Filegate probes real reflink support. `auto` enables only when every mount
  supports it; `on` uses byte-copy fallback where needed.
- Automatic captures happen on overwrite subject to the configured cooldown.
  Manual snapshots ignore the cooldown and are pinned by default.
- Raw filesystem writes do not create automatic versions.

Use the SDK `versions` client where available; see the
[versioning documentation](https://filegate.dev/docs/en/versioning) for
retention and operator details.

## Index maintenance

```http
POST /v1/index/rescan        ← force full rescan of all mounts
POST /v1/index/resolve       ← path↔id, single or batch (see "Stable file IDs")
```

You typically don't need `rescan` — the change detector handles external writes automatically. Use it after a known external bulk import, or in tests where you want to wait deterministically for convergence.

## Stats and observability

```http
GET /v1/stats
→ {
    "generatedAt": 1734567890,
    "index": { "totalEntities": 12345, "totalFiles": 12000, "totalDirs": 345, "dbSizeBytes": 67890123 },
    "cache": { "pathEntries": 1000, "pathCapacity": 10000, "pathUtilRatio": 0.1 },
    "mounts": [ ... ],
    "disks":  [ ... ]
  }

GET /health
→ 200 OK   ← no auth required, for liveness probes

GET /v1/health
→ dependency readiness: index, detector staleness, mount reachability

GET /v1/system/info
→ effective detector/versioning choices and mount capability probes
```

Use `/v1/system/runtime` for pollable live counters. `/v1/stats` is capacity
and metadata information, not a readiness probe.

## Conflict handling

Every write surface that can hit a name collision uses the same vocabulary, with `error` as the default. This is uniform — see [conflict-handling.md](conflict-handling.md) for the full matrix.

| Endpoint                         | Default | Allowed                |
|----------------------------------|---------|------------------------|
| `PUT /v1/paths`                  | error   | error/overwrite/rename |
| `POST /v1/nodes/{id}/mkdir`      | error   | error/skip/rename      |
| `POST /v1/uploads/sessions`      | error   | error/overwrite        |
| `POST /v1/transfers`             | error   | error/overwrite/rename |

On 409 the response body **may** carry `existingId` and `existingPath` for
diagnostic UIs — populated whenever the daemon could resolve the colliding
node (path-PUT, mkdir, upload session create/commit, transfers). A few generic conflict
paths (segment duplicate-content rejects, fallback envelope) return only
`{"error": "conflict"}`. See [`conflict-handling.md`](conflict-handling.md).

## Authentication

A single bearer token, resolved at daemon startup. When bootstrap config leaves
it empty, Filegate generates one, persists it in the runtime config store, and
prints it once. Empty never means unauthenticated REST. Send it on authenticated
`/v1/*` requests:

```
Authorization: Bearer <token>
```

That's it — there is no per-user auth, no scopes, no OAuth. Filegate is a backend-service dependency. If you need per-user authorization, do it in your own backend that fronts Filegate (relay pattern — see [relay-patterns.md](relay-patterns.md)).

## Free-space safety

Filegate has a configurable `min_free_bytes` guard. The upload-session
`/start` endpoint checks it explicitly and returns `507 Insufficient
Storage` upfront — saves bandwidth on rejected uploads. The one-shot `PUT
/v1/paths` endpoint does not pre-check the guard, but actual ENOSPC errors
during write are mapped to 507 too.

## What Filegate does NOT do

- **No multi-user authentication.** Single bearer token. Per-user logic goes in your backend.
- **No active-active or shared index.** One daemon owns a runtime/index store
  and writable mount set; there is no replication or leader election.
- **No transactions / multi-file atomicity.** Each operation is independent. There is no "upload these 5 files atomically".
- **No S3 object versioning.** REST has per-file versioning when configured, but the S3 surface does not expose object versions.
- **No user-visible file locks.** Filegate serializes its own write paths, but it does not expose leases or locks that clients can hold across separate operations.
- **No webhooks / event subscriptions.** The internal event bus is in-process only. If you want to react to filesystem events, you poll or you run inside the daemon.
- **No symlink following across mounts.** Symlinks pointing outside their mount are not resolved (security).
- **No native Windows / macOS support.** Linux only — relies on Linux xattr semantics for ID stability.
- **No trash / soft delete.** `DELETE` is permanent.
