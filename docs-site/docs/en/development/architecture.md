---
title: Development
section: Reference
order: 30
description: Code ownership, persistence and verification for the root API.
---

# Development

`domain` owns root operations, write publication, sessions, identities and
versions. Its `Files` and `State` ports are implemented by `infra/filesystem`
and `infra/pebble`. `adapter/http` owns wire parsing, bearer authentication and
signed capabilities. `cli` loads static YAML and owns process lifecycle.
`api/v1` exposes wire aliases and request envelopes; the SDKs follow this contract.

Persisted state separates derived `i/GENERATION/PATH` index rows from durable
`identity/`, `current/`, `v/`, `session/`, `done/`, `pending/` and `mutation/`
records. A rebuild writes a fresh index generation and switches it only after
success. Identity claims and version/session records survive. Recovery intents
make filesystem publication and its later state update restartable.

The filesystem adapter rejects symlink components and uses descriptor-relative
Linux operations. Indexed files carry UUIDs in `user.filegate.id`; durable inode
claims prevent copied attributes from transferring history. Per-root locks
serialize publication and maintenance. Network bodies are staged before taking
that lock. Cross-root transfers acquire both root locks in deterministic order.

```sh
make test
make test-linux
make test-race # Linux, CGO enabled for the race runtime
make docs
```

Integration tests cover real Linux xattrs, copied identities, confined paths,
rebuilds, version failure, metadata revision mapping, durable sessions and
publication recovery. Test API changes through both SDKs. Keep the installed
skill at `skills/filegate` identical to `docs-site/agent-skills/filegate`.

Hard-cut removals should disappear from code, configuration, clients, tests and
docs together. There is no legacy endpoint, schema migration, S3 adapter,
filesystem detector or admin application to maintain.
