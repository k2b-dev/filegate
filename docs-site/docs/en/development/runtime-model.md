---
title: Runtime model and guarantees
navTitle: Runtime model
section: Development
order: 310
description: Filegate sources of truth, consistency boundaries, and write semantics.
tags: [development, consistency, storage]
---

# Runtime model

This document defines Filegate's runtime behavior and guarantees.

## Operating contract

- Linux-only runtime support
- Bearer token auth for `/v1/*`, except scoped direct upload/download URLs
- Single-node and single-tenant operation
- Exactly one active daemon per runtime/index store and writable mount set
- No replication, leader election, shared-index mode, or multi-file transactions

## Sources of Truth

| State | Authority | Notes |
|---|---|---|
| File bytes, paths, ownership, modes, and stable IDs | Configured filesystem roots and `user.filegate.id` xattrs | Backups must preserve xattrs. |
| Applied manifest and generated credentials | Runtime config store | Not rebuildable from the file tree or metadata index. |
| Directory listings, path/ID lookup, and upload sessions | Pebble metadata index | Rebuildable from the filesystem except for in-progress session metadata. |
| Recent activity and live counters | Process memory | Lost on restart; not an audit log. |

The runtime config store and metadata index are separate Pebble databases with
different recovery semantics. Neither may be shared by multiple active
daemons.

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
- S3 writes: immediate visibility through the same indexed write path.
- External filesystem writes: eventual consistency via detector sync.
- Unknown detector scopes can trigger mount-scoped rescan fallback.
- Periodic full reconciliation (default `24h`, configurable or disableable)
  bounds how long detector blind spots can leave the index incomplete.

Detector cost model:

- btrfs backend consumes generation deltas (`subvolume find-new`) and is usually much cheaper for external-change ingestion.
- non-btrfs backends rely on polling and repeated directory/file checks, which becomes significantly more expensive as subtree size grows.

Each successful file operation publishes its result atomically at that
operation's boundary. Filegate does not provide a transaction spanning several
files or API calls, and separate reads are not a point-in-time snapshot of a
changing tree.

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

HTTP and S3 overwrites participate in automatic version capture when versioning
is active. Raw filesystem changes made by `cp`, `rsync`, or another process do
not create automatic Filegate versions.

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

## Configuration Model

- Bootstrap configuration is resolved from defaults, YAML, environment, and
  serve flags before the runtime store can be opened.
- The manifest is complete desired state for manifest-owned keys. Plan/apply
  always uses the authenticated HTTP API, locally or remotely.
- Runtime keys affect subsequent requests after apply. Static keys are stored
  as desired state but need a process restart before becoming effective.
- Secrets and S3 keys are not manifest keys. Generated bearer and S3 credentials
  live in the runtime config store and must be backed up.

## Filesystem and Versioning Model

- Every configured root must be a writable Linux directory with user xattr
  support. ext4, XFS, and btrfs are valid choices when those requirements hold.
- The btrfs detector is an optimization for external-change ingestion; polling
  is the portable fallback.
- Watched btrfs roots must not contain nested subvolumes. Externally deleting a
  nested subvolume can stall parent generation processing until daemon restart.
- Reflink support is probed with the kernel `FICLONE` operation, not inferred
  from the filesystem name.
- `versioning.enabled=auto` enables versioning only when all configured mounts
  support reflinks. `on` enables it with reflink, mixed, or byte-copy behavior;
  `off` disables it.
- Version blobs are Filegate-private data under `.fg-versions`. S3 clients do
  not receive an S3 object-versioning API.

## Network and Authorization Boundaries

- Filegate does not terminate TLS. A private network or reverse proxy owns TLS
  and exposure policy.
- One REST bearer token grants full file and config authority. There are no
  REST path scopes or read-only REST credentials.
- S3 keys can be bucket-scoped and rate-limited. REST has no built-in request
  limiter.
- The Admin app is a separate full-authority operator surface. Its Filegate
  token stays server-side; browser transfers use scoped URLs.
- Activity is a bounded in-memory aid, not durable compliance evidence.

## Performance Assumptions

- Metadata-heavy workloads should be index/cache dominated.
- Large upload/download throughput is primarily storage I/O bound.
- Worker pools and queue sizes control heavy async job pressure.
