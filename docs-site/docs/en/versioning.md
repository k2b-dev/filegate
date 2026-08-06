---
title: Versioning
navTitle: Versioning
section: Operate
order: 120
description: Use per-file version history on supported mounts.
tags: [versioning, btrfs]
---

# Versioning

Versioning is for operators and applications that need per-file history for
writes mediated by Filegate on supported filesystems.

## Scope

| Item | Scope | Meaning |
|---|---:|---|
| Automatic capture | Per file | Captures versions for REST and S3 overwrite paths, subject to cooldown and size rules. |
| Manual snapshot | Per file | Captures current bytes immediately and pins the version. |
| Restore | Per file | Restores in place or creates a new sibling file. |
| Retention | Per service config | Prunes unpinned versions according to retention buckets. |
| Filesystem support | Service selection and per mount copy | Filegate probes real `FICLONE` support. Reflinks are efficient; forced mode falls back to byte copies where unavailable. |

External writes through `cp`, `rsync`, or shell tools are not automatic
Filegate version captures. REST and S3 overwrite paths do participate in the
same capture hook, but S3 does not expose an object-versioning API.

## Config modes

| Mode | Scope | Meaning |
|---|---:|---|
| `auto` | Service | Enable only when every configured mount passes the reflink probe; otherwise disable globally. Default. |
| `on` | Service | Enable on every mount; use reflinks where supported and byte-copy fallback elsewhere. |
| `off` | Service | Disable versioning globally. |

Read `GET /v1/system/info` or the Admin System page after startup. It reports
the effective copy mode as `reflink`, `mixed`, `byte-copy`, or `disabled`, plus
the selection reason. Reflink support is a probed capability, not an assumption
based on the filesystem name.

## Basic operations

List versions:

```sh
curl -fsS -H 'Authorization: Bearer dev-token' \
  http://127.0.0.1:8080/v1/nodes/<node-id>/versions
```

Create a manual snapshot:

```sh
curl -fsS -X POST \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"label":"before migration"}' \
  http://127.0.0.1:8080/v1/nodes/<node-id>/versions/snapshot
```

Restore as a new file:

```sh
curl -fsS -X POST \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"asNewFile":true,"name":"restored.txt"}' \
  http://127.0.0.1:8080/v1/nodes/<node-id>/versions/<version-id>/restore
```

## Retention buckets

Retention buckets define age windows and maximum retained counts inside each window.

```yaml
versioning:
  retention_buckets:
    - keep_for: "1h"
      max_count: -1
    - keep_for: "24h"
      max_count: 24
    - keep_for: "720h"
      max_count: 30
```

| Field | Type | Scope | Meaning |
|---|---|---:|---|
| `keep_for` | duration | Retention bucket | Age window from now. |
| `max_count` | integer | Retention bucket | Versions retained in the window. `-1` means unlimited. |

Pinned versions are protected until the configured pin cap or post-delete grace rules apply.

For storage layout, capture ordering, pruning, and restore internals, see the
[versioning internals reference](/docs/en/reference/versioning-internals).
