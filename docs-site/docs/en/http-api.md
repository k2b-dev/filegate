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
relative path; `.` addresses the root where supported. IDs are optional metadata,
not the universal operation address.

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
`mode`, `uid` and `gid`. Timestamps are RFC 3339, sizes are integer bytes. Directory
size is zero; recursive totals belong to stats. `limit` defaults to 100 and is
bounded by 1000. Search/stats traversal defaults to 100,000 entries and accepts
an explicit maximum of 10,000,000.

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
404 for missing files, 409 for conflicts/disabled features, 413 for limits and
503 for concurrent transfer capacity. Do not retry a mutation blindly after an
ambiguous transport failure: session commits are idempotent, while ordinary
mutations require reading back the resulting state.
