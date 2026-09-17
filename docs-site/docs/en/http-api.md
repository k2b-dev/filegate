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
| `GET /archive` | `path` | TAR stream. |
| `GET /thumbnail` | `path`, `width`, `height` | JPEG preview. |
| `POST /directories` | `{path,ownership?}` | Created Node. |
| `PATCH /ownership` | `path`; body `{uid?,gid?,mode?,dirMode?}` | Updated Node. |
| `GET /acl` | `path`, `scope=access\|default` | `{entries}`. |
| `PUT /acl` | `path`, `scope=access\|default`; body `{entries}` | Stored ACL. |
| `DELETE /acl` | `path`, `scope=default` | 204. |
| `DELETE /files` | `path`, `recursive` | 204. |
| `POST /transfers` | `{path,targetRoot,targetPath,move?,onConflict?,ownership?,metadata?}` | Destination Node. |
| `POST /uploads/direct` | `{path,size,expiresIn?,onConflict?,ownership?,metadata?}` | `{url,method,expires}`. |
| `POST /downloads/direct` | `{path,expiresIn?}` | `{url,method,expires}`. |
| `POST /uploads/sessions` | `{path,size,onConflict?,ownership?,metadata?}` | Session plus scoped `url`. |
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

## Scoped session URL

Use the exact URL returned at session creation:

| Method | Meaning |
| --- | --- |
| `GET URL` | Status and received segment hashes. |
| `PUT URL?segment=N` | Exact segment bytes. |
| `POST URL` | Commit; repeated commits return the recorded result. |
| `DELETE URL` | Abort/retire the session. |

Raw stream routes return normal HTTP statuses. Common JSON statuses are 400 for
invalid input, 401 for authentication/capability failure, 403 for permissions,
404 for missing files, 409 for conflicts/disabled features, 413 for limits,
501 for unsupported POSIX ACLs and 503 for concurrent transfer capacity. Do not retry a mutation blindly after an
ambiguous transport failure: session commits are idempotent, while ordinary
mutations require reading back the resulting state.
