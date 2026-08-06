---
title: HTTP routes reference
navTitle: HTTP routes
section: Reference
order: 220
description: Complete Filegate REST route reference.
tags: [reference, http]
---

# HTTP routes reference

This reference catalogs Filegate HTTP routes for developers using the REST API directly or through SDKs.

Request and response type fields are documented in [HTTP JSON types reference](/docs/en/reference/http-types).

## Common request rules

| Rule | Scope | Meaning |
|---|---:|---|
| Auth | `/v1/*` routes | `Authorization: Bearer <token>` required except scoped direct token routes. |
| Error body | JSON error responses | `{ "error": "..." }`; conflicts can include `existingId` and `existingPath`. |
| Node IDs | Node routes | Stable IDs stored in Filegate metadata and xattrs. |
| Paths | Path routes | Virtual paths are relative to configured mount roots. |

## Routes

| Method | Path | Auth | Request | Response | Meaning |
|---|---|---|---|---|---|
| `GET` | `/health` | No | None | `OK` | Health check. |
| `GET` | `/v1/stats` | Bearer | None | `StatsResponse` | Runtime stats snapshot. |
| `GET` | `/v1/capabilities` | Bearer | None | `CapabilitiesResponse` | Runtime upload limits. |
| `GET` | `/v1/activity` | Bearer | Query filters | `ActivityListResponse` | Recent activity records. |
| `GET` | `/v1/paths/` | Bearer | None | `NodeListResponse` | List configured root nodes. |
| `GET` | `/v1/paths/{path...}` | Bearer | Listing query | `Node` | Resolve path and optionally include children. |
| `PUT` | `/v1/paths/{path...}` | Bearer | Binary body | `Node` + headers | One-shot upload by virtual path. |
| `GET` | `/v1/nodes/{id}` | Bearer | Listing query | `Node` | Read node metadata by ID. |
| `GET` | `/v1/nodes/{id}/content` | Bearer | `inline` query | Binary or tar stream | Download file or directory. |
| `GET` | `/v1/nodes/{id}/thumbnail` | Bearer | `size` query | Image bytes | Generate or read thumbnail. |
| `POST` | `/v1/nodes/{id}/mkdir` | Bearer | `MkdirRequest` | `Node` | Create child directory. |
| `PUT` | `/v1/nodes/{id}` | Bearer | Binary body | `Node` | Replace file content. |
| `PATCH` | `/v1/nodes/{id}` | Bearer | `UpdateNodeRequest` | `Node` | Rename or update ownership. |
| `DELETE` | `/v1/nodes/{id}` | Bearer | None | `204` | Delete node or subtree. |
| `POST` | `/v1/transfers` | Bearer | `TransferRequest` | `TransferResponse` | Move or copy a node. |
| `GET` | `/v1/search/glob` | Bearer | Search query | `GlobSearchResponse` | Glob search over indexed paths. |
| `POST` | `/v1/uploads/direct` | Bearer | `DirectUploadURLRequest` | `DirectUploadURLResponse` | Mint one-shot scoped PUT URL. |
| `PUT` | `/v1/uploads/direct/{token}` | Scoped token | Binary body | Headers + status | Upload through direct URL. |
| `POST` | `/v1/downloads/direct` | Bearer | `DirectDownloadURLRequest` | `DirectDownloadURLResponse` | Mint scoped GET URL. |
| `GET` | `/v1/downloads/direct/{token}` | Scoped token | None | Binary or tar stream | Download through direct URL. |
| `HEAD` | `/v1/downloads/direct/{token}` | Scoped token | None | Headers | Check direct download target. |
| `POST` | `/v1/uploads/sessions` | Bearer | `UploadSessionCreateRequest` | `UploadSessionResponse` | Create resumable upload session. |
| `POST` | `/v1/uploads/sessions:batch` | Bearer | `UploadSessionBatchCreateRequest` | `UploadSessionBatchCreateResponse` | Create multiple sessions atomically. |
| `GET` | `/v1/uploads/sessions/{sessionId}` | Bearer or scoped token | None | `UploadSessionResponse` | Read session status. |
| `PUT` | `/v1/uploads/sessions/{sessionId}/segments/{index}` | Bearer or scoped token | Binary body | `UploadSegmentResponse` | Upload one segment. |
| `POST` | `/v1/uploads/sessions/{sessionId}/commit` | Bearer or scoped token | None | `UploadSessionCommitResponse` | Assemble and commit session. |
| `DELETE` | `/v1/uploads/sessions/{sessionId}` | Bearer or scoped token | None | `204` | Abort session. |
| `POST` | `/v1/index/rescan` | Bearer | None | `OKResponse` | Schedule or run index rescan. |
| `POST` | `/v1/index/resolve` | Bearer | `IndexResolveRequest` | Resolve response | Resolve paths or IDs in bulk. |
| `GET` | `/v1/nodes/{id}/versions` | Bearer | `cursor`, `limit` | `ListVersionsResponse` | List file versions. |
| `GET` | `/v1/nodes/{id}/versions/{vid}/content` | Bearer | None | Binary stream | Download version bytes. |
| `POST` | `/v1/nodes/{id}/versions/snapshot` | Bearer | `VersionSnapshotRequest` | `VersionResponse` | Capture pinned manual snapshot. |
| `POST` | `/v1/nodes/{id}/versions/{vid}/pin` | Bearer | `VersionPinRequest` | `VersionResponse` | Pin version or update label. |
| `POST` | `/v1/nodes/{id}/versions/{vid}/unpin` | Bearer | None | `VersionResponse` | Clear pinned flag. |
| `POST` | `/v1/nodes/{id}/versions/{vid}/restore` | Bearer | `VersionRestoreRequest` | `VersionRestoreResponse` | Restore version in place or as new file. |
| `DELETE` | `/v1/nodes/{id}/versions/{vid}` | Bearer | None | `204` | Delete one version. |

## Operations

Read-only service state, plus the two operator actions that are safe to trigger on demand. These back the admin System page.

| Method | Path | Auth | Request | Response | Meaning |
|---|---|---|---|---|---|
| `GET` | `/v1/system/info` | Bearer | None | `SystemInfoResponse` | Build, mounts, and effective limits. Probes the filesystem, so not for polling. |
| `GET` | `/v1/system/runtime` | Bearer | None | `SystemRuntimeResponse` | Live counters: detector, worker pool, caches, upload sessions, retention. Safe to poll. |
| `GET` | `/v1/health` | Bearer | None | `HealthResponse` | Dependency health. Answers `503` when a check fails, with the body still present. |
| `GET` | `/v1/uploads/sessions` | Bearer | `phase` query | `UploadSessionListResponse` | List upload sessions, optionally by phase. |
| `POST` | `/v1/versions/prune` | Bearer | None | `PruneResponse` | Run a retention round now. Answers `409` while a round is in flight. |

`/health` is the unauthenticated liveness probe and only reports that the process is up. `/v1/health` is the authenticated dependency check.

## Administration

These change service behavior and credentials. They exist only when the server was started with a config manager and an S3 key service, which `fg serve` always does.

| Method | Path | Auth | Request | Response | Meaning |
|---|---|---|---|---|---|
| `GET` | `/v1/config/schema` | Bearer | None | `ConfigSchemaResponse` | Every key with type, activation, owner, unit, default, and restart reason. |
| `GET` | `/v1/config` | Bearer | None | `ConfigValuesResponse` | Manifest metadata plus effective, desired, and provenance. Secrets report presence only. |
| `POST` | `/v1/config/plan` | Bearer | `ConfigManifestPlanRequest` | `ConfigManifestPlanResponse` | Validate and diff a complete replacement manifest without changing state. |
| `POST` | `/v1/config/apply` | Bearer | `ConfigManifestApplyRequest` | `ConfigManifestApplyResponse` | Replace desired state when `expectedRevision` still matches. Returns `409` for a stale plan. |
| `GET` | `/v1/s3/keys` | Bearer | None | `S3KeyListResponse` | List S3 access keys. Secrets are never included. |
| `POST` | `/v1/s3/keys` | Bearer | `S3KeyCreateRequest` | `S3KeyCreated` | Create a key. The secret is returned once and never again. |
| `PATCH` | `/v1/s3/keys/{accessKey}` | Bearer | `S3KeyUpdateRequest` | `S3Key` | Change buckets, rate limits, or label. |
| `POST` | `/v1/s3/keys/{accessKey}/rotate` | Bearer | None | `S3KeyCreated` | Issue a new secret for an existing key. |
| `DELETE` | `/v1/s3/keys/{accessKey}` | Bearer | None | `204` | Delete a key. Deleting the last key leaves nothing able to authenticate against S3. |

`values` is a map of dotted config paths to values. Omission removes a previously managed path; `null`, bootstrap-owned, secret, resource-owned, and unknown keys are rejected. The bearer token grants configuration authority as well as data access. See [Security model](/docs/en/security).

## Listing query

| Name | Type | Default | Scope | Meaning |
|---|---|---:|---:|---|
| `pageSize` | integer | `100` | Directory listing | Maximum child entries returned. |
| `cursor` | string | empty | Directory listing | Opaque `nextCursor` from previous page. |
| `computeRecursiveSizes` | boolean | `false` | Directory metadata | Computes recursive directory sizes. |
| `fingerprint` | enum | `cached` | File metadata | `cached` returns stored fingerprints, `none` omits `etag` and `sha256`, `ensure` computes and stores SHA-256 for file nodes when missing. |

## Mutation query

| Name | Type | Default | Scope | Meaning |
|---|---|---:|---:|---|
| `onConflict` | enum | `error` | `PUT /v1/paths/{path...}` | `error`, `overwrite`, or `rename`. |
| `recursiveOwnership` | boolean | `true` | `PATCH /v1/nodes/{id}`, `POST /v1/transfers` | Applies ownership updates to directory descendants. |

## Search query

| Name | Type | Default | Scope | Meaning |
|---|---|---:|---:|---|
| `pattern` | string | required | Search request | Glob pattern. |
| `paths` | string list | all roots | Search request | Comma or semicolon separated base paths. |
| `limit` | integer | server default | Search request | Maximum results. |
| `showHidden` | boolean | `false` | Search request | Include hidden files and directories. |
| `files` | boolean | `true` | Search request | Include files. |
| `directories` | boolean | `false` | Search request | Include directories. |

## Activity query

| Name | Type | Default | Scope | Meaning |
|---|---|---:|---:|---|
| `limit` | integer | `100` | Activity query | Maximum records returned. Values above `1000` are capped. |
| `offset` | integer | `0` | Activity query | Zero-based pagination offset inside retained records. |
| `q` | string | empty | Activity query | Case-insensitive search over operation, actor, target, request ID, error, and metadata text. |
| `operation` | string | empty | Activity query | Exact operation filter. Empty means all operations. |
| `outcome` | string | empty | Activity query | Exact outcome filter. Empty means all outcomes. |
