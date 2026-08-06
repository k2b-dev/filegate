---
title: HTTP API
navTitle: HTTP API
section: APIs
order: 70
description: Use the Filegate REST API for path, node, upload, download, transfer, search, stats, activity, and version operations.
tags: [http, rest, api]
---

# HTTP API

The HTTP API is for application servers, tools, and SDKs that need the full Filegate feature set.

## Base rules

| Rule | Scope | Meaning |
|---|---:|---|
| API prefix | Service listener | All REST routes except `/health` are under `/v1`. |
| Authentication | `/v1/*` routes | Use `Authorization: Bearer <token>`. |
| Public health | Service listener | `GET /health` returns `OK` without auth. |
| Scoped direct URLs | One upload/download URL | Direct upload and download URLs carry their own scoped token. |
| Error envelope | Failed JSON route | `{ "error": "..." }`; conflicts can include `existingId` and `existingPath`. |

## Basic requests

List roots:

```sh
curl -fsS -H 'Authorization: Bearer dev-token' \
  http://127.0.0.1:8080/v1/paths/
```

Upload a file:

```sh
curl -fsS -X PUT \
  -H 'Authorization: Bearer dev-token' \
  --data-binary @photo.jpg \
  'http://127.0.0.1:8080/v1/paths/data/photo.jpg?onConflict=rename'
```

Read service stats:

```sh
curl -fsS -H 'Authorization: Bearer dev-token' \
  http://127.0.0.1:8080/v1/stats
```

## Conflict handling

| Mode | Scope | Meaning |
|---|---:|---|
| `error` | Upload, mkdir, transfer | Return `409 Conflict` when the target exists. Default. |
| `overwrite` | File upload and transfer targets | Replace an existing file or transfer target according to endpoint rules. |
| `rename` | One-shot uploads, mkdir, transfers | Create a unique sibling name and return the created node. |
| `skip` | `mkdir` only | Return the existing directory if it already exists. |

REST file-write and transfer routes default to `error`; clients must choose a non-default conflict mode before Filegate replaces existing data.

## Route groups

| Group | Routes | Use for |
|---|---|---|
| Health and stats | `/health`, `/v1/stats`, `/v1/capabilities`, `/v1/activity` | Service state and operator introspection. |
| Paths | `/v1/paths/...` | Virtual path lookup, directory listing, and one-shot uploads. |
| Nodes | `/v1/nodes/{id}...` | ID-based metadata, content, thumbnails, metadata updates, and deletion. |
| Transfers | `/v1/transfers` | Move and copy operations. |
| Search | `/v1/search/glob` | Indexed glob search. |
| Upload sessions | `/v1/uploads/sessions...` | Resumable and direct browser uploads. |
| Direct uploads | `/v1/uploads/direct` | Short-lived one-shot browser PUT URLs. |
| Direct downloads | `/v1/downloads/direct` | Short-lived browser GET URLs. |
| Versions | `/v1/nodes/{id}/versions...` | Per-file version listing, snapshots, pinning, restore, and purge. |

See [HTTP routes reference](/docs/en/reference/http-routes) for every route and [HTTP JSON types reference](/docs/en/reference/http-types) for request and response fields.
