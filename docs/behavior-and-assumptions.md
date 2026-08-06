# Behavior and Assumptions

This document lists core runtime assumptions and behavioral guarantees.

## Scope

- Linux-only runtime support
- No backward-compatibility guarantee with legacy TS-only implementation
- Bearer token auth for `/v1/*`, except scoped direct upload/download URLs

## Virtual Filesystem Model

- `storage.base_paths` defines mounted roots.
- HTTP root (`/v1/paths/`) exposes those roots as virtual entries.
- Path operations are constrained to configured roots.

## Metadata and Index Model

- Metadata source of truth for API reads is Pebble index.
- Index format has explicit version gate.
- Startup may rebuild index if format is incompatible.
- `rescan` (including `--new`) is the primary maintenance operation.
- Manual index recovery is available via `filegate index rescan --new` (offline operation).

## Consistency Model

- HTTP writes: immediate visibility in metadata reads.
- External filesystem writes: eventual consistency via detector sync.
- Unknown detector scopes can trigger mount-scoped rescan fallback.
- Periodic full reconciliation (default `24h`, configurable or disableable)
  bounds how long detector blind spots can leave the index incomplete.

Detector cost model:

- btrfs backend consumes generation deltas (`subvolume find-new`) and is usually much cheaper for external-change ingestion.
- non-btrfs backends rely on polling and repeated directory/file checks, which becomes significantly more expensive as subtree size grows.

## Read Behavior

- `GET /v1/paths/{path...}` and `GET /v1/nodes/{id}` return metadata.
- Directory metadata responses may include paginated children.
- `GET /v1/nodes/{id}/content`:
  - file node -> file stream
  - directory node -> tar stream
- Direct signed download: `GET /v1/downloads/direct/{token}` after `POST /v1/downloads/direct`

## Write Behavior

- One-shot upload: `PUT /v1/paths/{path...}`
- Direct signed upload: `PUT /v1/uploads/direct/{token}` after `POST /v1/uploads/direct`
- Node content replace: `PUT /v1/nodes/{id}` (file nodes only)
- Directory creation: `POST /v1/nodes/{id}/mkdir`
- Metadata update: `PATCH /v1/nodes/{id}`
- Delete subtree: `DELETE /v1/nodes/{id}`

## Upload Session Semantics

- Session metadata is durable in Pebble
- Segments may arrive out-of-order
- Duplicate segment sends are accepted if content matches
- Commit is explicit and verifies the final checksum before publish
- Staging is mount-local in `.fg-uploads`

## Ownership Semantics

- Ownership payload uses `ownership { uid, gid, mode, dirMode }`
- Recursive ownership application is explicit through API parameters
- Transfer operations can apply ownership behavior recursively

## Safety and Limits

- JSON body size limits are enforced
- Upload size/segment limits are enforced
- Uploads may return `507 insufficient storage` when free space falls below configured safety threshold
- Path traversal and root escape are rejected
- Symlink escape protections are part of security tests

## Performance Assumptions

- Metadata-heavy workloads should be index/cache dominated.
- Large upload/download throughput is primarily storage I/O bound.
- Worker pools and queue sizes control heavy async job pressure.
