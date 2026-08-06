# S3-Compatible API

Filegate exposes an S3-compatible HTTP API alongside its native REST API. The S3 listener is **disabled by default**; enable it in config (`s3.enabled: true`) and configure at least one credential.

The implementation targets the subset of the AWS S3 API that real-world clients use for backup, sync, and object-store workflows — `rclone`, `restic`, `kopia`, `awscli`, `Bun.s3`, Cyberduck, MinIO Client, etc. It is **not** a full AWS-compatibility layer.

This page documents what's implemented, the deviations from AWS, and the limits.

---

## Path style only

Filegate uses **path-style addressing**:

```
http://filegate.local:9100/{bucket}/{key}
```

Virtual-hosted-style (`{bucket}.s3.example.com`) is not supported. Configure your client for path-style explicitly — most clients have a flag for this (see [s3-clients.md](./s3-clients.md)).

A bucket maps 1:1 to a configured Filegate mount; the mount name is the bucket name. Bucket-name validation runs at startup when `s3.enabled=true`: every mount name must satisfy AWS S3 bucket rules (3-63 chars, lowercase alphanumeric + hyphens, no IP-like names, no AWS-reserved prefixes/suffixes, not `.fg-versions` or `.fg-uploads`).

CreateBucket / DeleteBucket are intentionally **rejected** — buckets are operator-configured, not client-provisioned.

---

## Authentication

Every request is signed with **AWS Signature Version 4 (SigV4)**. Filegate accepts both:

- **Header-mode**: `Authorization: AWS4-HMAC-SHA256 Credential=…`
- **Query-mode (presigned URLs)**: `?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=…`

The streaming chunked-payload mode (`x-amz-content-sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD`) is supported for PUT/POST bodies.

Region is configurable (`s3.region`). Clients must use the same region in their credential scope. Operators who don't care about region should keep `us-east-1`.

Authorization is per-key: each key has an explicit bucket whitelist. ListBuckets is filtered to the requesting key's set, and any access to a bucket outside the whitelist returns `AccessDenied` — bucket existence is **never leaked** for forbidden buckets. See [s3-config.md](./s3-config.md) for the key-store schema.

---

## Implemented Operations

### Service-level

| Op            | Method | Path | Notes |
|---------------|--------|------|-------|
| ListBuckets   | GET    | `/`  | Filtered to the key's bucket whitelist. |

### Bucket-level

| Op                     | Method | Path                       | Notes |
|------------------------|--------|----------------------------|-------|
| HeadBucket             | HEAD   | `/{bucket}`                | 200 if accessible, 403 if forbidden, 404 if non-existent. |
| ListObjectsV2          | GET    | `/{bucket}?list-type=2`    | Supports `prefix`, `delimiter`, `start-after`, `continuation-token`, `max-keys`, `encoding-type=url`. |
| ListMultipartUploads   | GET    | `/{bucket}?uploads`        | Returns in-progress + committing uploads sorted oldest-first. No pagination yet. |
| DeleteObjects          | POST   | `/{bucket}?delete`         | Bulk delete. Up to 1000 keys per call, parallelized. Quiet mode supported. |
| CreateBucket / DeleteBucket | PUT / DELETE | `/{bucket}` | **Rejected** — buckets come from filegate config. |

### Object-level

| Op                     | Method | Path                       | Notes |
|------------------------|--------|----------------------------|-------|
| PutObject              | PUT    | `/{bucket}/{key}`          | Body is the object content. Honors `If-Match`, `If-None-Match: *`, `Content-MD5`, `Content-Type`, `Content-Encoding`, `Content-Disposition`, `x-amz-meta-*` (≤ 2 KiB total). |
| GetObject              | GET    | `/{bucket}/{key}`          | Returns body + S3-shape headers. Supports `Range:`, conditional `If-Match`/`If-None-Match`/`If-Modified-Since`/`If-Unmodified-Since`. |
| HeadObject             | HEAD   | `/{bucket}/{key}`          | Same headers as GetObject, no body. |
| DeleteObject           | DELETE | `/{bucket}/{key}`          | Idempotent (404 → 204). Honors `If-Match`. |
| CopyObject             | PUT    | `/{bucket}/{key}` + `x-amz-copy-source` | Server-side single-object copy. ≤ 5 GiB; same-mount reflink fast-path when supported. Supports `x-amz-copy-source-if-{match,none-match,modified-since,unmodified-since}`, `x-amz-metadata-directive: COPY|REPLACE`. |
| CreateMultipartUpload  | POST   | `/{bucket}/{key}?uploads`  | Captures content-type + user-metadata for the eventual Complete. |
| UploadPart             | PUT    | `/{bucket}/{key}?partNumber=N&uploadId=X` | Per-part body, returns ETag. Optional `Content-MD5`. |
| ListParts              | GET    | `/{bucket}/{key}?uploadId=X` | Lists uploaded parts in ascending PartNumber order. |
| AbortMultipartUpload   | DELETE | `/{bucket}/{key}?uploadId=X` | Idempotent staging-dir cleanup. |
| CompleteMultipartUpload | POST  | `/{bucket}/{key}?uploadId=X` | Validates + concats parts, atomic 2-phase commit. Returns the composite ETag (`<hex(MD5(concat-of-part-MD5-bytes))>-<N>`). |

### Not implemented

- Bucket lifecycle, versioning, replication, ACLs, policies, CORS, encryption keys.
- Object tagging, legal hold, retention, presigned-POST forms.
- ListObjectsV1 (`GET /{bucket}` without `list-type=2`) — clients should use V2.
- ListObjectVersions (filegate exposes no per-object S3 versions).
- UploadPartCopy (multipart-copy). The single-call CopyObject covers ≤ 5 GiB; oversized sources currently return `EntityTooLarge`.
- SelectObjectContent (S3 Select).

Any unimplemented op returns `NotImplemented` (501) with a clear error message.

---

## Deviations from AWS

These are intentional differences in semantics:

### Bucket existence isolation

A request to a bucket that the requesting key is not authorized for returns **403 AccessDenied** — never 404 — regardless of whether the bucket exists. This prevents bucket-name probing. Once authorization passes, a genuinely non-existent bucket does return 404 `NoSuchBucket` (operator debugging would otherwise be misleading).

### NoSuchUpload code

A multipart op against an unknown `uploadId` returns 404 `NoSuchUpload` (per AWS spec).

### CopyObject ETag

The destination's stored `ETagMD5` and the response `<ETag>` are the **whole-body MD5** of the copied bytes. When the source was uploaded multipart, the source's composite ETag (`...-N`) is still used for source preconditions (`x-amz-copy-source-if-match` etc.) — that's what the client originally got from the multipart Complete — but the destination presents itself as a fresh single-MD5 object. This is the AWS behavior for single-call CopyObject.

### MultipartETag retention

Multipart-uploaded objects retain their composite ETag (`...-N`) on subsequent S3 GET/HEAD. **Any non-S3 write (REST adapter, detector-driven sync, rescan) clears `MultipartETag`** and resets the file to a single-MD5 identity. Cross-protocol consistency requires the file to look "fresh" after non-S3 mutation.

### x-amz-meta-* limit

Filegate enforces a **2 KiB** ceiling on the total user-metadata blob (matches AWS spec). Per-header values are joined with a comma when the same key repeats.

### VersionId rejected

`x-amz-copy-source: /bucket/key?versionId=…` and `<Object><VersionId>…</VersionId></Object>` in DeleteObjects are explicitly rejected (`InvalidArgument` per entry, or top-level for CopyObject). Filegate's REST-side per-file versioning is **not** exposed on the S3 surface.

### Multipart cleanup

Successful multipart Complete deletes the staging `parts/` and `complete.tmp` immediately. Active multipart metadata lives in Pebble; part bytes stay under `.fg-uploads` until Complete, Abort, or cleanup. A durable Pebble record is written only after Complete succeeds and is used for idempotent Complete retries. The cleanup loop retires `phase=done` active rows and their durable records after the configured done-retention window, retires aborted legacy manifests after aborted-retention, and force-aborts stale `in_progress` uploads after the stuck-upload max age. `phase=committing` uploads are not cleanup-eligible; startup recovery reconciles them against the durable record.

If startup recovery finds a `phase=committing` upload without a durable record, it logs the upload ID, bucket, key, and staging path, then leaves the active row in place. This means the original Complete did not reach the final Pebble commit. The safe recovery path is to retry `CompleteMultipartUpload` with the original parts list; if the client is gone and the upload is intentionally abandoned, an operator may remove the logged staging directory after confirming the object was not created.

### `If-None-Match` on PutObject / CopyObject

Only `If-None-Match: *` is supported (the AWS-defined create-only form). A specific ETag value returns `InvalidArgument`.

### TLS termination

The S3 listener speaks plain HTTP. Production deployments **must** put a reverse proxy (Traefik, Caddy, nginx) in front for TLS — SigV4 already authenticates the request body, so plain-HTTP between the proxy and filegate is safe inside a trusted network.

---

## Limits

| Limit                                 | Value          | Notes |
|---------------------------------------|----------------|-------|
| Object size (single PUT)              | unbounded by S3 config | Limited by filesystem capacity and server write timeout; REST `upload.max_upload_bytes` does not apply to S3 PutObject. |
| Multipart parts per upload            | 10,000         | AWS spec hard limit. |
| Multipart part size (non-final)       | ≥ 5 MiB        | Final part may be smaller. |
| CopyObject single-call source size    | 5 GiB          | Above → `EntityTooLarge`; UploadPartCopy not yet implemented. |
| User metadata total                   | 2 KiB          | Sum of all `x-amz-meta-*` header bytes after JSON encoding. |
| DeleteObjects keys per call           | 1,000          | AWS spec. |
| ListObjectsV2 max-keys                | 1,000          | Default 1,000; clamped to that ceiling. |
| Presigned URL expiry                  | 7 days         | AWS hard limit. |
| Clock skew tolerance                  | 15 minutes     | Matches AWS. |

---

## Reserved namespaces

Object keys starting with these segments are reserved by filegate and rejected with `InvalidArgument`:

- `.fg-versions/` — internal version storage (REST-only versioning feature).
- `.fg-uploads/` — internal multipart-upload staging.

These segments also cannot appear inside a key path (e.g. `dir/.fg-uploads/file.bin` is rejected).

---

## See also

- [s3-config.md](./s3-config.md) — config schema, key store, mount mapping, TLS.
- [s3-clients.md](./s3-clients.md) — known-good config snippets for popular clients.
- [behavior-and-assumptions.md](./behavior-and-assumptions.md) — cross-protocol consistency rules.
