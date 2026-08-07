---
title: TypeScript client deep reference
navTitle: TypeScript client
section: Deep reference
order: 290
description: Complete TypeScript construction, relay, upload, download, error, and versioning patterns.
tags: [reference, typescript, sdk]
---

# TypeScript Client (In-Depth)

This document describes the intended stateless TS client pattern for Filegate.

## Goals

- Stateless client construction
- Scoped namespaces for every authenticated REST area
- Relay-first streaming APIs
- Minimal runtime surprises across server and browser

## Two Construction Modes

### Mode A: default env-based instance (server/runtime only)

Use this on Node/Bun server runtimes where env vars exist.

```ts
import { filegate } from "@k2b/filegate/client";

process.env.FILEGATE_URL = "http://127.0.0.1:8080";
process.env.FILEGATE_TOKEN = "dev-token";

const roots = await filegate.paths.get();
const caps = await filegate.capabilities.get();
```

Behavior expectation:

- default instance is created lazily on first use
- reads `FILEGATE_URL` and `FILEGATE_TOKEN`
- safe to import without immediate side effects

### Mode B: explicit instance

Use this when you want explicit dependency injection. Keep the Filegate bearer
token in trusted server runtimes; browser uploads/downloads should use scoped
direct URLs instead.

```ts
import { Filegate } from "@k2b/filegate/client";

const fg = new Filegate({
  baseUrl: "https://filegate.internal.example",
  token: "<token>",
  fetchImpl: fetch,
});
```

## Complete client surface

| Namespace | Methods |
|---|---|
| `paths` | `get`, `put`, `putRaw` |
| `nodes` | `get`, `contentRaw`, `putContent`, `mkdir`, `patch`, `delete`, `thumbnailRaw` |
| `uploads` | `createDirectUploadURL`; sessions: `create`, `createBatch`, `status`, `segments.put`, `segments.putRaw`, `commit`, `abort` |
| `downloads` | `createDirectURL` |
| `transfers` | `create` |
| `search` | `glob` |
| `index` | `rescan`, `resolvePath`, `resolvePaths`, `resolveId`, `resolveIds` |
| `stats` | `get` |
| `system` | `info`, `runtime`, `health`, `prune`, `uploadSessions` |
| `config` | `schema`, `values`, `plan`, `apply` |
| `s3Keys` | `list`, `create`, `update`, `rotate`, `delete` |
| `capabilities` | `get` |
| `versions` | `list`, `listAll`, `contentRaw`, `snapshot`, `pin`, `unpin`, `restore`, `delete` |
| `activity` | `list` |

The package also exports the browser helpers `upload`, `uploadDirect`, and
`directUploads`. Pure segment and checksum helpers live under
`@k2b/filegate/utils` rather than on the authenticated client.

## Core Usage

### List roots and stat a path

```ts
const roots = await fg.paths.get();
const node = await fg.paths.get("data/invoices/2026", { pageSize: 100 });
```

### Metadata by id and content stream

```ts
const meta = await fg.nodes.get(node.id, { pageSize: 100 });
const upstream = await fg.nodes.contentRaw(meta.id, { inline: false });
```

### One-shot upload

```ts
await fg.paths.put("data/uploads/hello.txt", new TextEncoder().encode("hello\n"), {
  contentType: "text/plain",
});
```

### Conflict handling

Every write defaults to `onConflict: "error"` — the server never silently
overwrites. To replace an existing file, pass `"overwrite"` explicitly. To
make the upload land under a different name when the target exists, pass
`"rename"`:

```ts
import { FilegateError } from "@k2b/filegate/client";

// File upload
try {
  await fg.paths.put("photos/sunset.jpg", bytes);
} catch (e) {
  if (e instanceof FilegateError && e.status === 409 && e.errorResponse) {
    // e.errorResponse.existingId / existingPath tell the UI what's there.
    const overwrite = await confirmFromUser(e.errorResponse.existingPath);
    await fg.paths.put("photos/sunset.jpg", bytes, {
      onConflict: overwrite ? "overwrite" : "rename",
    });
  } else throw e;
}

// mkdir — `skip` is the idempotent-folder pattern (mkdir -p style)
await fg.nodes.mkdir(parentId, { path: "uploads", onConflict: "skip" });

// Upload session — resumable, parallel, explicit commit
await fg.uploads.sessions.create({
  path: "data/videos/video.mp4",
  size,
  checksum,
  segmentSize: 8 << 20,
  onConflict: "error",
});
```

Available modes per endpoint:

| Endpoint                  | Modes                          |
|---------------------------|--------------------------------|
| `paths.put` / `paths.putRaw` | `error`, `overwrite`, `rename` |
| `nodes.mkdir`             | `error`, `skip`, `rename`      |
| `uploads.sessions.create` | `error`, `overwrite`           |
| `transfers.create`        | `error`, `overwrite`, `rename` |

### Upload session

Pure segment math and hashing live at `@k2b/filegate/utils`. They do
not require a `Filegate` instance, so they are safe in browsers, Web Workers,
and any environment that does not have a token.

```ts
import { uploads } from "@k2b/filegate/utils";

const bytes = new Uint8Array(10 * 1024 * 1024);
const checksum = await uploads.checksum.sha256(bytes);
const segmentSize = 1024 * 1024;

const session = await fg.uploads.sessions.create({
  path: "data/video.bin",
  size: bytes.byteLength,
  checksum,
  segmentSize,
});

for (const segment of session.segments) {
  const bytesForSegment = bytes.slice(segment.offset, segment.offset + segment.size);
  await fg.uploads.sessions.segments.put({
    sessionId: session.id,
    index: segment.index,
    body: bytesForSegment,
    checksum: await uploads.checksum.sha256(bytesForSegment),
  });
}

const committed = await fg.uploads.sessions.commit({ sessionId: session.id });
console.log("done", committed.node.id);
```

Notes:

- segment order does not matter
- duplicate segment PUTs are allowed when content matches
- commit is explicit and safe to retry after success

### Direct browser upload

Use this when your app architecture is:

```text
browser <-> app/RBAC server <-> filegate
```

The app server keeps the Filegate bearer token, creates a short-lived upload
URL, and returns it to the browser. Configure `server.public_url` on Filegate
when the public URL differs from the internal listener URL.

For cross-origin browser uploads, configure CORS at your reverse proxy. If
Filegate must handle it directly, set `server.cors.allowed_origins`; the
default is disabled.

Server-side minting:

```ts
const direct = await fg.uploads.createDirectUploadURL({
  path: "data/inbox/photo.jpg",
  contentType: "image/jpeg",
  expiresInSeconds: 15 * 60,
  onConflict: "rename",
  maxBytes: 50 * 1024 * 1024,
});

return Response.json({ uploadUrl: direct.uploadUrl });
```

Browser-side upload:

```ts
import { uploadDirect } from "@k2b/filegate/client";

await uploadDirect(uploadUrlFromYourServer, file, {
  onSuccess: async ({ node }) => {
    await fetch("/api/uploads/complete", {
      method: "POST",
      body: JSON.stringify({ filegateId: node.id }),
    });
  },
  onError: async (error) => {
    await fetch("/api/uploads/failed", { method: "POST", body: String(error) });
  },
  onFinish: async (outcome) => {
    console.log(outcome.ok ? "done" : "failed");
  },
});
```

The signed URL is scoped to one virtual path, conflict mode, expiry, content
type, and byte limit. It is a bearer credential until it expires; do not log it
as a durable secret. For large browser uploads, create an upload session with
`direct: {}` on your backend and return the `direct` object. The browser can
then call `directUploads.segments.put`, `directUploads.status`, and
`directUploads.commit` without the Filegate bearer token.

### Direct browser download

Use this when the browser should download from Filegate directly, while your
app server keeps the Filegate bearer token.

```ts
const direct = await fg.downloads.createDirectURL({
  nodeId: "<node-id>",
  expiresInSeconds: 5 * 60,
});

return Response.redirect(direct.downloadUrl, 303);
```

The signed URL supports `GET`, `HEAD`, and byte ranges for files. Directories
download as tar streams.

### Versions

Per-file version history is managed through REST and available when versioning
is enabled. S3 overwrites can participate in capture, but S3 does not expose
object versioning.
Automatic mode requires reflink support on every mount; explicit `on` mode can
use full byte copies.

```ts
const page = await fg.versions.list("<file-id>", { limit: 20 });
const snapshot = await fg.versions.snapshot("<file-id>", "before migration");

await fg.versions.pin("<file-id>", snapshot.versionId, "keep");
const restored = await fg.versions.restore("<file-id>", snapshot.versionId, {
  asNewFile: true,
  name: "restored.bin",
});

console.log(restored.node.id, page.items.length);
```

### Effective storage modes

Use the system endpoint to explain effective detector and versioning behavior
without inferring it from configuration:

```ts
const info = await fg.system.info();

console.log(info.detector.configuredBackend, info.detector.backend, info.detector.reason);
console.log(info.versioning.enabled, info.versioning.copyMode, info.versioning.reason);
for (const mount of info.mounts) console.log(mount.path, mount.reflinkSupported);
```

Operational callers can also read cheap runtime counters, inspect readiness,
list resumable sessions, and trigger retention:

```ts
const runtime = await fg.system.runtime();
const health = await fg.system.health();
const sessions = await fg.system.uploadSessions({ phase: "in_progress" });
const pruned = await fg.system.prune();
```

### Declarative configuration

`config.plan` and `config.apply` accept the complete managed values map. Always
plan first and pass the observed revision to apply so concurrent operators
cannot overwrite each other silently.

```ts
const current = await fg.config.values();
const desired = { "metrics.enabled": true };
const plan = await fg.config.plan(desired);

if (plan.changes.length > 0) {
  await fg.config.apply(desired, plan.currentRevision);
}
```

### S3 access keys

S3 secrets are returned only by `create` and `rotate`. Store the returned
secret immediately.

```ts
const created = await fg.s3Keys.create({ buckets: ["data"] });
await fg.s3Keys.update(created.accessKey, { requestsPerSecond: 20, burst: 40 });
const rotated = await fg.s3Keys.rotate(created.accessKey);
await fg.s3Keys.delete(created.accessKey);
```

## Relay/Proxy Pattern

### Upload passthrough

```ts
const upstream = await fg.paths.putRaw(virtualPath, request.body, {
  contentType: request.headers.get("content-type") ?? "application/octet-stream",
});

return new Response(upstream.body, {
  status: upstream.status,
  headers: upstream.headers,
});
```

### Download passthrough

```ts
const upstream = await fg.nodes.contentRaw(nodeId, { inline: true });
return new Response(upstream.body, { status: upstream.status, headers: upstream.headers });
```

## Error Model

Normalize errors to include:

- `status`
- `message` (prefer parsed `{ error: string }`)
- `method`
- `path`

This is critical for backend observability under load.

## Browser Notes

- Do not rely on `process.env` defaults in browser bundles.
- Do not expose the Filegate bearer token in browser bundles.
- Use `uploadDirect(...)` for direct uploads that should bypass your app server's request body path.
- Use `fg.downloads.createDirectURL(...)` server-side, then redirect the browser for direct downloads.

## Contract Source of Truth

Server JSON contracts live under:

- [`api/v1`](https://github.com/k2b-dev/filegate/tree/main/api/v1)

TypeScript declarations are maintained in `sdk/ts/src/types.ts`; they are not
generated. API changes therefore need matching SDK types, client methods, and
contract tests in the same change.
