---
title: Internal architecture
navTitle: Architecture
section: Development
order: 300
description: Understand Filegate modules, runtime flow, index layout, and repair paths.
tags: [development, architecture, pebble]
---

# Architecture

## High-level Modules

- `cmd/filegate`: executable entrypoint
- `cli`: cobra/viper command tree
  - local service, health/status and index admin commands
- `adapter/http`: HTTP routing/middleware, request/response mapping
- `domain`: business rules and orchestration
- `infra/pebble`: metadata index backend
- `infra/filesystem`: filesystem I/O adapter
- `infra/detect`: external filesystem change detection
- `infra/jobs`: bounded worker scheduler for heavy async work
- `infra/fgbin`: binary metadata record codec used in index values
- `sdk/filegate`: Go client SDK

## Runtime Flow

1. `filegate serve` loads config (`/etc/filegate/conf.yaml` default).
2. Core service is created (`domain.Service`) with:
   - Pebble index
   - filesystem store
   - in-memory event bus
   - bounded in-memory activity ring buffer
3. Initial index scan occurs through service/index logic.
4. Detector backend (`auto|poll|btrfs`) produces FS events.
5. HTTP adapter serves `/v1/*` and maps directly to domain operations.

Detector behavior by filesystem/backend:

- `btrfs`: reads generation deltas via `btrfs subvolume find-new` and resolves changed inodes to paths.
- `poll` (typical on ext4/xfs): relies on directory/file polling (`readdir`/`lstat` patterns) because no btrfs-like change journal API exists.

Watched btrfs roots must not contain nested subvolumes. Externally deleting a
nested subvolume can stall generation processing for the parent until the
daemon restarts; configure those subvolumes as separate roots instead.
- Independent of the backend, a configurable daily full reconciliation uses
  the existing serialized rescan path to repair any missed namespace changes.

Write semantics:

- HTTP-triggered writes are synchronously committed to index and FS.
- External FS writes are eventual-consistent through detector sync/rescan.

## Pebble Layout

Key families:

- Entity key: `0x01 + <16-byte fileID>`
- Child key: `0x02 + <16-byte parentID> + <kind-byte> + 0x00 + <name-bytes>`
  - `kind-byte` ordering enforces dirs before files in listings

Additional reserved meta key:

- index format version key (`index_format_version` guard)

See implementation in [`infra/pebble/index.go`](https://github.com/ValentinKolb/filegate/blob/main/infra/pebble/index.go).

## fgbin Record Format (Index Values)

`infra/fgbin` encodes compact binary records.

Entity v2 core fields:

- `id`, `parentId`, `isDir`, `size`, `mtimeNs`
- `uid`, `gid`, `mode`, `device`, `inode`, `nlink`
- `name`, `mimeType`
- extension list (`fieldId`, `len`, `value`)

Current extension usage:

- `fieldId=1` for EXIF payload (JSON blob)
- `fieldId=2` for the single-part/cross-protocol MD5 ETag
- `fieldId=3` for the S3 composite multipart ETag
- `fieldId=4` for explicit S3 `Content-Type`
- `fieldId=5` for S3 `Content-Encoding`
- `fieldId=6` for S3 `Content-Disposition`
- `fieldId=7` for serialized S3 user metadata

`DecodeEntity` enforces version/type/canonical extension ordering to avoid ambiguous decoding.

## Security-relevant Design Points

- Virtual path sanitization and root confinement in domain/path helpers
- Bearer auth enforced for `/v1/*`, except signed direct-upload PUT URLs
- Directory content download as tar stream with preflight checks
- Ownership/mode updates via explicit API fields (no implicit elevation path)
- Max body limits for JSON and upload endpoints

## Performance-relevant Design Points

- Metadata path/id reads are index-first
- Path and reverse path caches in domain service
- Bounded worker queues for expensive jobs (thumbnail generation)
- Pooled copy buffers for stream endpoints
- Upload-session staging in mount-local `.fg-uploads`

## Failure/Repair Paths

- Index format mismatch triggers explicit rebuild behavior
- Detector batch apply falls back to rescan on hard failure
- Offline index reset flow: `filegate index rescan --new` (expects daemon stopped)
