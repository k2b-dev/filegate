---
title: TypeScript SDK
navTitle: TypeScript SDK
section: APIs
order: 80
description: Use the TypeScript SDK from Node, Bun, and browser-assisted applications.
tags: [typescript, sdk]
---

# TypeScript SDK

The TypeScript SDK is for Node, Bun, and browser-assisted applications that call Filegate through typed client helpers.

## Install

```sh
npm i @valentinkolb/filegate
```

## Server-side client

```ts
import { Filegate } from "@valentinkolb/filegate";

const client = new Filegate({
  baseUrl: "http://127.0.0.1:8080",
  token: "dev-token",
});

const roots = await client.paths.get("");
```

## Main namespaces

| Namespace | Scope | Use for |
|---|---:|---|
| `paths` | Virtual filesystem paths | Resolve paths, list directories, upload by path. |
| `nodes` | Stable node IDs | Read metadata, content, thumbnails, updates, deletes. |
| `uploads` | Upload sessions and direct upload URLs | Large uploads, browser direct uploads, session status, commit, abort. |
| `downloads` | Direct download URLs | Browser-safe direct downloads. |
| `transfers` | Node move/copy operations | Move or copy a node to a target parent. |
| `search` | Indexed search | Glob search across indexed paths. |
| `index` | Service index | Trigger rescans and resolve paths or IDs in bulk. |
| `versions` | Per-file versions | List, snapshot, pin, unpin, restore, delete versions. |
| `stats` | Runtime stats | Service, index, cache, mount, disk, and process state. |
| `system` | Operational state | Health, effective storage modes, live workers, upload sessions, and retention pruning. |
| `config` | Declarative configuration | Inspect schema and values, plan manifests, and apply desired state. |
| `s3Keys` | S3 credentials | Create, update, rotate, disable, and delete access keys. |
| `activity` | Activity ring buffer | Recent operation events. |
| `capabilities` | Runtime limits | Upload and transfer limits for adaptive clients. |

## Browser upload helper

Use `upload()` when the browser should upload files directly to Filegate while the app server mints scoped sessions.

```ts
import { upload } from "@valentinkolb/filegate";

await upload({
  files,
  path: "/data/inbox",
  allow: (request) =>
    fetch("/api/uploads/sessions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    }).then((res) => res.json()),
  config: {
    onConflict: "skip-existing",
  },
  onEvent(event) {
    console.log(event.type);
  },
});
```

The browser helper does not need the Filegate bearer token. The `allow` callback is implemented by your application server.

### Small files skip the session protocol

Files at or below `config.directThresholdBytes` are announced to `allow` with
`kind: "direct"` and uploaded with a single scoped PUT instead of create, segment
and commit. It defaults to `segmentSize`, so a file that would have been one
segment goes direct with no configuration.

Your `allow` endpoint has to handle both kinds. A direct item carries no
`checksum` and no `segments`, and it expects a `{ kind: "direct", direct }`
directive minted from `POST /v1/uploads/direct`; a session item expects a
`session`. [Upload commit measurements](/docs/en/development/benchmarks/results/commit-cost) put the
one-shot path at 2.4x the throughput of sessions for small files.

Pass `directThresholdBytes: 0` to send everything through sessions. The shortcut
is also skipped when `onConflict` is `skip-identical`, which needs the checksum
only the session path computes.

## Conflict defaults

| API | Default conflict behavior | Meaning |
|---|---|---|
| Server REST calls | `error` | Return a conflict unless the caller opts into another mode. |
| Browser `upload()` helper | Caller config | The helper sends the configured mode to the app server allow endpoint. |
| Upload sessions | `error` | Resumable sessions accept `error` or `overwrite`. |

For the complete method inventory, framework integration patterns, and raw
response helpers, see the [TypeScript client reference](/docs/en/reference/typescript-client).
