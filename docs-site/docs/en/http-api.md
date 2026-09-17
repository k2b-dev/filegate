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
| `GET /entries` | `path`, `after`, `limit` | `{items,next?}`. |
| `GET /search` | `q`, `path`, `after`, `limit`, `maxEntries` | Filename substring matches. |
| `GET /content` | `path` | File bytes, Range/HEAD supported. |
| `GET /thumbnail` | `path`, `width`, `height` | JPEG preview. |
| `POST /directories` | `{path,ownership?,acl?:{access?,default?}}` | Created Node; parent must exist. |
| `PATCH /ownership` | `path`; body `{uid?,gid?,mode?,dirMode?}` | Updated Node. |
| `GET /acl` | `path`, `scope=access\|default` | `{entries}`. |
| `PUT /acl` | `path`, `scope=access\|default`; body `{entries}` | Stored ACL. |
| `DELETE /acl` | `path`, `scope=default` | 204. |
| `DELETE /files` | `path`, `recursive` | 204. |
| `POST /transfers` | `{path,targetRoot,targetPath,move?,onConflict?,ownership?,metadata?}` | Destination Node. |
| `POST /uploads/direct` | `{path,size,expiresIn?,onConflict?,ownership?,metadata?}` | `{url,method,expires}`. |
| `POST /downloads/direct` | `{path,expiresIn?}` | `{url,method,expires}`. |
| `POST /uploads/sessions` | `{path,size,expiresIn?,allowAbort?,onConflict?,ownership?,metadata?}` | `{session,lease}`. |
| `GET /uploads/sessions/{id}` | — | Backend session status and optional commit result. |
| `POST /uploads/sessions/{id}/lease` | `{expiresIn?,allowAbort?}` | `{url,expires,operations}`. |
| `POST /uploads/sessions/{id}/commit` | — | Original committed Node. |
| `DELETE /uploads/sessions/{id}` | — | Abort; 204. |
| `GET /index` | — | Index status. |
| `POST /index/rebuild` | — | Final index status. |
| `GET /stats` | — | Cached recursive stats or null. |
| `POST /stats/refresh` | `maxEntries` | Recursive stats. |
| `GET /versions` | `path` | Versions, newest first. |
| `POST /versions` | `path`; body `{pinned?,metadata?}` | Manual version. |
| `PATCH /versions/{id}` | `path`; body `{pinned,metadata?}` | Updated version attributes. |
| `DELETE /versions/{id}` | `path` | 204. |
| `GET /versions/{id}/content` | `path` | Version bytes. |
| `POST /versions/{id}/restore` | `path` | Current Node. |
| `POST /versions/prune` | — | `{deleted}`. |

A Node contains `root`, `path`, optional `id`, `directory`, `size`, `modified`,
`mode`, `uid` and `gid`. Mode is an octal string including special bits, such as
`"2770"` for a setgid directory. Timestamps are RFC 3339, sizes are integer bytes. Directory
size is zero; recursive totals belong to stats. `limit` defaults to 100 and is
bounded by 1000. Search/stats traversal defaults to 100,000 entries and accepts
an explicit maximum of 10,000,000.

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
`id`, `root`, `path`, `size`, `chunkSize`, `expires`, `state`, `options`, `segments`
and `received`. Terminal sessions also expose `terminalAt`, `retainUntil` and,
when committed, the original Node in `result`. Sessions last 24 hours, independent
of their lease lifetime. Renewing a lease requires backend authentication and an
open session; it does not extend the session deadline.

Use the exact session lease URL:

| Method | Required lease operation | Meaning |
| --- | --- | --- |
| `GET URL` | `status` | Transfer state and received segment hashes. |
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
