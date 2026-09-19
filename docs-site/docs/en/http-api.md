---
title: HTTP API
section: Reference
order: 10
description: Root-scoped routes, request shapes and response conventions.
---

# HTTP API

All `/v1/*` routes require `Authorization: Bearer TOKEN`, except signed
`/v1/direct/{token}` routes. `GET /health` is unauthenticated. JSON errors have
`{"error":"code","message":"description"}`. Bodies use camelCase; configuration
uses snake_case. Unknown JSON fields are rejected.

Root operations have prefix `/v1/roots/{root}`. A `path` query parameter is a
relative path; `.` addresses the root where supported. Indexed roots also
provide file IDs. Use `GET /resolve?id=ID` to find a file's current path.

| Method and suffix | Input | Result |
| --- | --- | --- |
| `GET /v1/system` | — | Build, uptime, maintenance health. |
| `GET /v1/roots` | — | Root information array. |
| `GET /v1/roots/{root}` | — | Root information. |
| `GET /stat` | `path` | Node. |
| `GET /resolve` | `id` | Node, indexed roots only. |
| `GET /entries` | `path`, `sort`, `order`, `type`, `after`, `limit`, `maxEntries` | `{items,next?}`. |
| `GET /search` | `q`, `path`, `sort`, `order`, `type`, `after`, `limit`, `maxEntries` | Filename substring matches. |
| `GET /content` | `path`, optional `fileName` | File bytes, Range/HEAD supported. |
| `GET /thumbnail` | `path`, `width`, `height` | JPEG preview. |
| `POST /directories` | `{path,ownership?,acl?:{access?,default?}}` | Created Node; parent must exist. |
| `PATCH /ownership` | `path`; body `{uid?,gid?,mode?,dirMode?}` | Updated Node. |
| `GET /acl` | `path`, `scope=access\|default` | `{entries}`. |
| `PUT /acl` | `path`, `scope=access\|default`; body `{entries}` | Stored ACL. |
| `DELETE /acl` | `path`, `scope=default` | 204. |
| `DELETE /files` | `path`, `recursive` | 204. |
| `POST /transfers` | `{path,targetRoot,targetPath,move?,id?,...WriteOptions}` | `TransferResult`; 202 when pending. |
| `POST /uploads/direct` | `{path,size,expiresIn?,...WriteOptions}` | `{url,method,expires}`. |
| `POST /downloads/direct` | `{path,expiresIn?,fileName?}` | `{url,method,expires}`. |
| `POST /versions/{id}/downloads/direct` | `{path,expiresIn?,fileName?}` | Version lease: `{url,method:"GET",expires}`. |
| `POST /thumbnail/direct` | `{path,width?,height?,expiresIn?}` | Thumbnail lease: `{url,method:"GET",expires}`. |
| `POST /uploads/sessions` | `{path,size,expiresIn?,allowAbort?,idempotencyKey?,...WriteOptions}` | `{session,lease?}`. |
| `GET /uploads/sessions/{id}` | — | Backend session status and optional commit result. |
| `POST /uploads/sessions/{id}/lease` | `{expiresIn?,allowAbort?}` | `{url,expires,operations}`. |
| `POST /uploads/sessions/{id}/commit` | — | Original committed Node. |
| `DELETE /uploads/sessions/{id}` | — | Abort; 204. |
| `GET /index` | — | Index status. |
| `POST /index/rebuild` | — | Final index status. |
| `GET /stats` | — | Cached recursive stats or null. |
| `POST /stats/refresh` | `path?`, `maxEntries` | Subtree observation, or refreshed root stats. |
| `GET /versions` | `path` | Versions, newest first. |
| `POST /versions` | `path`; body `{pinned?,metadata?}` | Manual version. |
| `PATCH /versions/{id}` | `path`; body `{pinned,metadata?}` | Updated version attributes. |
| `DELETE /versions/{id}` | `path` | 204. |
| `GET /versions/{id}/content` | `path`, optional `fileName` | Version bytes, Range/HEAD supported. |
| `POST /versions/{id}/restore` | `path` | Current Node. |
| `POST /versions/prune` | — | `{deleted}`. |

A Node contains `root`, `path`, optional `id`, `directory`, `size`, `modified`,
`mode`, `uid`, `gid` and optional managed `revision`. Mode is an octal string including special bits, such as
`"2770"` for a setgid directory. Timestamps are RFC 3339, sizes are integer bytes. Directory
size is zero; recursive totals belong to stats. `limit` defaults to 100 and is
bounded by 1000. Listing/search traversal defaults to 100,000 entries, also its
maximum. Stats uses an explicit traversal budget. [Browsing](/docs/en/browsing)
describes cursor validity, sorting/filtering and completeness.
Listing revisions may be absent or stale; use current stat or current-content
ETag when selecting an `ifMatch` condition.

`WriteOptions` contains optional `onConflict`, `ownership`, `accessACL`, `metadata`
and `precondition`. An explicit `accessACL` replaces the destination file's access
ACL; a requested mode is applied afterward and can change its effective mask.
`precondition` is `{ifMatch: "opaque-revision"}` or `{ifNoneMatch: true}`. It requires
a managed root and is bound into upload leases and sessions. Mismatch returns
`412 precondition_failed`; the standard JSON error shape remains `{error,message}`.
See [conditional publication](/docs/en/uploads-downloads#publish-only-if-unchanged).

Additional transfer and segment routes use the same root prefix:

| Method and suffix | Input | Result |
| --- | --- | --- |
| `GET /uploads/sessions/{id}/segments` | `after=-1`, `limit=100` | `{items:[{index,hash}],next?}`. |
| `GET /transfers/{id}` | Destination root | `TransferResult`. |
| `POST /transfers/{id}/resume` | Destination root | Result; 202 while pending. |
| `POST /transfers/{id}/abandon` | Destination root | Terminal result; no file deletion. |
| `POST /versions/{id}/copy` | `{path,targetRoot,targetPath,...WriteOptions}` | 201 destination Node. |

`TransferResult` has `state`, optional `node`, `id` and `sourceRoot`. Cross-root
moves require an application-generated UUID `id` and two managed roots. Copies
and same-root moves reject `id`. See [transfer recovery](/docs/en/transfers).

## Unix execution header

An authenticated backend can bind a technical Unix identity to file operations:

```http
X-Filegate-Execution: {"uid":10001,"gid":20001,"groups":[20002]}
```

`uid` and `gid` are required integers. UID is 1–4294967294; GIDs are
0–4294967294. `groups` is an optional array of at most 64 supplementary GIDs,
normalized by sorting and removing duplicates. The header accepts one JSON
object, no unknown fields, and at most 4096 bytes. The root must advertise
`execution: true`.

The same identity applies to source and destination roots in transfers and to
all roots in an archive selection. Direct upload, download, historical and
thumbnail leases sign the identity. Session creation persists it in the backend
session's optional `execution` field. A new session lease or commit uses that
stored identity. An unscoped backend can manage the session, but cannot change
its execution identity by omitting the header.

| Condition | Response |
| --- | --- |
| Invalid identity/header | 400 `invalid_argument`. |
| Execution header on a direct URL | 400 `execution_override_not_allowed`. |
| Execution header on an administrative route | 400 `execution_not_supported`. |
| Session header differs from its stored identity | 403 `execution_mismatch`. |
| Kernel denies access | 403 `forbidden`. |
| Root has execution disabled | 409 `feature_disabled`. |
| Execution capacity exhausted | 503 `execution_capacity`, with `Retry-After: 1`. |

Administrative routes that reject the header are system information, root lists
and root information, search, index status/rebuild, stats/read/refresh and version
pruning. Their results are not filtered by the identity. File operations,
including ownership and ACL routes, accept it. See
[permissions](/docs/en/permissions#unix-execution-identity) for the exact native
permission semantics and private provisioning behavior.

## POSIX ACLs

ACL routes require both `path` and `scope=access|default`. They also support `.`
for the root itself. Default ACLs apply only to directories. PUT replaces one
scope and returns its stored ACL; DELETE supports only `scope=default`.

```json
{
  "entries": [
    { "tag": "owner", "permissions": "rwx" },
    { "tag": "owningGroup", "permissions": "rwx" },
    { "tag": "group", "id": 20002, "permissions": "r-x" },
    { "tag": "mask", "permissions": "rwx" },
    { "tag": "other", "permissions": "---" }
  ]
}
```

Nonempty ACLs require exactly one `owner`, `owningGroup` and `other`. Named `user`
and `group` entries require a nonnegative numeric `id`, unique within the tag,
and an explicit `mask`. IDs range from 0 to 4294967294. PUT accepts 3–256 entries;
an empty array is invalid even for the default scope. Use DELETE to remove it.
Other tags do not accept `id`. Permission strings are
`rwx`, `rw-`, `r-x`, `r--`, `-wx`, `-w-`, `--x` or `---`.

Reading an access ACL returns at least its three base entries. Reading a default
ACL returns an empty entries array when none is present. Unsupported POSIX ACLs
return 501 (`acl_not_supported`). Invalid ACL input returns 400 (`invalid_acl`);
insufficient permissions return 403 (`forbidden`). Existing ACLs larger than
256 entries return 413 (`limit_exceeded`) when read, rather than being truncated.
ACLs work independently of indexing. They do not create versions or change
children recursively. See [permissions and ACLs](/docs/en/permissions) for mask
semantics, inheritance and shared-directory setup.

## Directory creation

`POST /directories` creates one new directory with optional ownership and access
and default ACLs. Each ACL is an `{entries}` object using the schema above. The
parent must exist. An existing target returns 409 without changing its metadata.
The target appears only after the requested permissions have been applied.
Staging and destination must share a filesystem; otherwise the operation returns
501 (`unsupported_storage_layout`). After a lost response, read the target state
before retrying. See [permissions](/docs/en/permissions) for an example.

## Transfer leases

`expiresIn` is an integer number of seconds, default 60, maximum 300. Zero also
selects the default. Expiry limits the start of new requests; accepted downloads
can continue. Leases are reusable and have no per-lease revocation. Upload write
options are bound at creation and cannot be changed through a lease.

Session creation returns separate `session` and `lease` objects. The session has
`id`, `root`, `path`, `size`, `chunkSize`, `expires`, `state`, `options`, `uploadedSegments`
and `received`. Terminal sessions also expose `terminalAt`, `retainUntil` and,
when committed, the original Node in `result`. Sessions last 24 hours, independent
of their lease lifetime. Renewing a lease requires backend authentication and an
open session; it does not extend the session deadline.

Use the exact session lease URL:

| Method | Required lease operation | Meaning |
| --- | --- | --- |
| `GET URL` | `status` | Compact transfer state and received counts. |
| `GET URL?segments=1&after=-1&limit=100` | `status` | Paged acknowledged segment hashes. |
| `PUT URL?segment=N` | `write` | Exact segment bytes; session must remain open. |
| `DELETE URL` | `abort` | Abort the session. |

`status` and `write` are always granted. `abort` is granted only when the backend
sets `allowAbort: true` for that lease. A lease never permits commit or renewal.
Browser status omits the target path, write options and commit result.

Session states are `open`, `committed`, `aborted` and `expired`. Terminal records
are retained for seven days after completion or the session expiry time.
Repeated commits return the original Node, even if its path no longer exists.
Aborting a committed session returns `409 session_committed`; repeated aborts
return 204. Writes and commits on aborted sessions return `409 session_aborted`;
expired sessions return `410 session_expired`. A missing record after retention
returns 404, which does not reveal whether the upload committed.

Session creation may be retried with the same `idempotencyKey` and identical
path, size, write options and execution identity while the record is retained.
A different request with the same key returns 409. Terminal replay returns the
session without a lease. Keys are root-scoped strings of at most 128 bytes,
without leading or trailing whitespace. Segment pages use an exclusive numeric
`after` index (-1 initially) and a limit of 1–1000; terminal pages are empty.

## Version and thumbnail download leases

Issue these leases from an authenticated backend. Both return HTTP 201 with
`{url,method:"GET",expires}`. The browser uses the returned URL with GET or HEAD,
without an Authorization header. Other data methods return 403
(`operation_not_allowed`). The same expiry and CORS rules apply to all direct URLs.

A version lease binds the root, relative path and version ID. The version must
belong to the file currently at that path. Moving or replacing that identity,
deleting the version or pruning it makes the lease unavailable; it does not
switch to another version or follow a renamed file.

A thumbnail lease binds the root, relative path and normalized `width` and
`height`. Each omitted dimension defaults to 256. Explicit dimensions must be
integers from 1 to 2048; zero is invalid. The preview fits within those bounds,
preserves aspect ratio and does not upscale. Output is always JPEG at quality 85,
with image orientation applied. The source is the file found at the signed path
when requested, so a replacement can change the preview. Query parameters added
to either lease URL cannot override the signed target, version or dimensions.

Direct and authenticated routes share the same response behavior:

| Content | Content-Type | Content-Disposition | Range |
| --- | --- | --- | --- |
| Current file | `application/octet-stream` | Attachment with filename | Supported; 206 or 416. |
| Historical version | `application/octet-stream` | Attachment with filename | Supported; 206 or 416. |
| Thumbnail | `image/jpeg` | Absent | Ignored; full image, 200. |

HEAD returns headers without a body. Allowed CORS origins can read
`Content-Length`, `Content-Range`, `Content-Disposition`, `ETag` and `Retry-After`. Successful content responses use
`Cache-Control: no-store`.
The authenticated version-content and thumbnail endpoints remain available.

Missing files or versions return 404 (`not_found`); disabled versioning returns
409 (`feature_disabled`). Invalid dimensions or unsupported image data return
400 (`invalid_argument`); sources over 64 MiB or 40 million pixels return
413 (`limit_exceeded`). Thumbnail issuance checks the image header and limits;
the download also decodes the full image, so corrupt or changed sources can
still fail. Thumbnail capacity exhaustion returns 503 (`thumbnail_capacity`) with
`Retry-After: 1`. At most four renders and 32 duplicate-render waiters are active;
each output is limited to 16 MiB. Every caller opens its source before sharing
an in-flight render. Results are not persistently cached.
Thumbnails require neither indexing nor versioning.

## ZIP selection leases

`POST /v1/downloads/archives` requires backend authentication:

```json
{
  "items": [
    { "root": "documents", "path": "report.pdf", "archivePath": "report.pdf" },
    { "root": "shared", "path": "photos", "archivePath": "photos" }
  ],
  "expiresIn": 60
}
```

The response is `{url,method:"POST",expires,manifest}`. Submit the exact returned
`manifest` string to `url` as a single `manifest` field in an
`application/x-www-form-urlencoded` body. The manifest hash is signed; altered
selections are rejected. The response streams an uncompressed ZIP with attachment
headers. ZIP downloads do not support HEAD or Range.

Selections may span roots. A selected directory includes its whole current
subtree; the backend must authorize that scope. Private Filegate entries are
excluded from traversal, and explicit private selections are rejected. Symlinks,
unsafe archive paths and duplicate, nested or case-colliding selection names are
rejected. Limits: 1,000 selections, 128 KiB manifest, 10,000 expanded entries,
20,000 scanned objects (including excluded private entries), depth 64, 100 GiB of
file contents and four concurrent archive streams. Archive names must be valid
UTF-8 and portable Windows-compatible relative paths. Collision checks normalize
Unicode and ignore case. Use `path: "."` to select a root; an empty path is invalid.
Read failures after response headers abort the stream. Selections are not snapshots.

## Errors and retries

Raw stream routes return normal HTTP statuses. Common JSON statuses are 400 for
invalid input, 401 for authentication/lease failure, 403 for permissions,
404 for missing files or receipts, 409 for conflicts/closed sessions/disabled
features, 410 for expired sessions, 413 for limits, 501 for unsupported POSIX ACLs
or storage layout, and 503 for concurrent transfer capacity. Do not retry a mutation blindly after an
ambiguous transport failure: session commits are idempotent while their receipts
are retained; ordinary mutations require reading back the resulting state.
