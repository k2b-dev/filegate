# TypeScript SDK

Package: `@k2b/filegate`. ESM only. Targets ES2022. Works in Node ≥ 18, Bun, Deno, and modern browsers.

## Construction — pick your runtime

There are two construction modes. Mixing them up causes 90% of integration bugs.

### Server runtime (Node / Bun) — env-based default

```ts
import { filegate } from "@k2b/filegate/client";

process.env.FILEGATE_URL = "http://127.0.0.1:8080";
process.env.FILEGATE_TOKEN = "dev-token";

// `filegate` is a lazy proxy — created on first property access from env vars.
const roots = await filegate.paths.get();
```

Properties:

- Lazy: importing has no side effects; the underlying client is constructed on first use.
- Reads `FILEGATE_URL` and `FILEGATE_TOKEN`. Throws clearly if either is missing.
- Single shared instance per process — fine because it's stateless.

### Server / trusted internal — explicit DI with `new Filegate(...)`

```ts
import { Filegate } from "@k2b/filegate/client";

const fg = new Filegate({
  baseUrl: "https://filegate.internal.example",
  token: "<token>",
  fetchImpl: fetch,                         // optional — defaults to globalThis.fetch
  userAgent: "my-app/1.2.3",                // optional
  defaultHeaders: { "X-App-Trace": "..." }, // optional, applied to every request
});

const roots = await fg.paths.get();
```

Use this when:

- You're in a server runtime but want explicit dependency injection (tests,
  multi-instance setups, instrumented `fetch`).
- You're in a **trusted internal** browser environment (e.g. an admin tool
  that already has access to the daemon token via the runtime, or
  CI-built deploy tools that hold the token at build time).

### Public browser apps — DO NOT construct `Filegate` directly

Filegate's bearer token grants full access to all data on the daemon.
There are no bearer-token scopes or per-user auth. Putting that token in
JavaScript that ships to end-users (a public web app, a mobile webview,
an embedded widget) is a full compromise the moment a single user opens
devtools.

For general API calls, run a **backend relay** in front of Filegate:

```
Browser (no token) ──HTTP──▶ Your backend (holds token) ──HTTP──▶ Filegate
```

For browser uploads/downloads, the backend can also mint direct URLs:

```ts
const direct = await fg.uploads.createDirectUploadURL({
  path: "data/inbox/photo.jpg",
  contentType: "image/jpeg",
  expiresInSeconds: 900,
});

const download = await fg.downloads.createDirectURL({
  nodeId: "<node-id>",
  expiresInSeconds: 300,
});
```

The browser then calls `uploadDirect(uploadUrl, file, handlers)` without a
Filegate bearer token, or follows the scoped `download.downloadUrl`.

The backend uses the SDK normally; the browser talks to your backend
endpoints with whatever auth you already have (sessions, JWT, OAuth, etc.)
or to scoped direct URLs. See
[`relay-patterns.md`](relay-patterns.md) for full upload/download patterns.
For purely-client-side helpers like file hashing or segment math (no Filegate
connection needed), import from `@k2b/filegate/utils` — those are
pure functions and ship without the HTTP client.

## Scoped namespaces

```ts
fg.paths        // PathsClient    — virtual paths (PUT, GET listings)
fg.nodes        // NodesClient    — ID-based ops
fg.uploads      // UploadsClient  — direct upload URLs and resumable sessions
fg.downloads    // DownloadsClient — direct download URLs
fg.transfers    // TransfersClient — move / copy
fg.search       // SearchClient   — glob
fg.index        // IndexClient    — rescan, resolve
fg.stats        // StatsClient    — daemon stats
fg.system       // SystemClient   — health, runtime, sessions, prune
fg.config       // ConfigClient   — schema, values, manifest plan/apply
fg.s3Keys       // S3KeysClient   — access-key lifecycle
fg.capabilities // CapabilitiesClient — runtime limits
fg.versions     // VersionsClient — per-file version history
fg.activity     // ActivityClient — recent operation history
fg.baseUrl      // string         — the configured base URL
```

One-shot uploads live under `fg.paths.put()`, **not** under `fg.uploads`.
`fg.uploads` contains direct upload URL minting and upload sessions.

Pure helpers ship under `@k2b/filegate/utils`:

```ts
import { uploads } from "@k2b/filegate/utils";
uploads.segments.count({ size, segmentSize });
uploads.segments.bounds(index, size, segmentSize);
await uploads.checksum.sha256(uint8Array);
```

## Common operations

### List mounts

```ts
import type { NodeListResponse } from "@k2b/filegate/client";

const result = await fg.paths.get();              // no path → root listing
const roots = result as NodeListResponse;         // narrow — see below
roots.items.forEach((m) => console.log(m.id, m.name, m.path));
```

`paths.get()` is the single browse method — there is no `paths.list()`.
Its return type is `Promise<Node | NodeListResponse>` — when called
without a path the runtime shape is `NodeListResponse`, with a path it's
`Node`. TypeScript needs an explicit narrowing or type guard:

```ts
const r = await fg.paths.get(virtualPath);
if ("items" in r) { /* roots listing */ } else { /* single Node */ }
```

### Get a node by virtual path or ID

```ts
const meta = await fg.paths.get("data/photos/2026", { pageSize: 100, cursor: "" });
const meta = await fg.nodes.get(nodeId, { pageSize: 100 });
```

For directories the response includes `children`, `pageSize`, `nextCursor`.

### One-shot upload

```ts
const res = await fg.paths.put("data/uploads/hello.txt", new TextEncoder().encode("hello\n"), {
  contentType: "text/plain",
  onConflict: "error",     // default — explicit shown for clarity
});
console.log(res.nodeId, res.createdId, res.node.path);
```

`createdId` is non-empty only when a new file was created (vs. overwriting
an existing one with `onConflict: "overwrite"`).

### Streaming download (for relays)

```ts
const resp = await fg.nodes.contentRaw(fileId, { inline: false });
// resp is a raw Response — including non-2xx. Pass body through to your
// own client unchanged (status + headers + body):
return new Response(resp.body, { status: resp.status, headers: resp.headers });
```

For directory IDs the body is a tar stream (`Content-Type: application/x-tar`).

`contentRaw`, `putRaw`, `thumbnailRaw`, and `uploads.sessions.segments.putRaw`
return the raw `Response` **without throwing on 4xx/5xx**. That's what makes
them usable for relay handlers — the upstream status reaches the downstream
client unchanged. The non-`Raw` variants throw `FilegateError` on non-2xx.

### Mkdir

```ts
await fg.nodes.mkdir(parentId, {
  path: "subdir/with/intermediates",
  recursive: true,
  ownership: { uid: 1000, gid: 1000, mode: "750", dirMode: "750" },
  onConflict: "skip",   // idempotent — return existing dir if already there
});
```

### Rename or change ownership

```ts
await fg.nodes.patch(nodeId, { name: "renamed.txt" });
await fg.nodes.patch(dirId, { ownership: { uid: 1001, gid: 1001, mode: "640" } }, true /* recursive */);
```

### Move or copy

```ts
await fg.transfers.create({
  op: "move",
  sourceId: srcId,
  targetParentId: parentId,
  targetName: "destination.bin",
  onConflict: "rename",          // "error" | "overwrite" | "rename"
});
```

(There is no `ensureUniqueName` field — that's just `onConflict: "rename"`.)

### Search

```ts
const res = await fg.search.glob({
  pattern: "**/*.{jpg,jpeg,png}",
  paths: ["data"],
  limit: 200,
  files: true,
  directories: false,    // server default is false; pass true to include dirs
});
```

### Thumbnail

Allowed sizes: **128, 256, 512** (default 256). Other values are rejected.

```ts
const resp = await fg.nodes.thumbnailRaw(imageId, { size: 256 });
const blob = await resp.blob();
imgEl.src = URL.createObjectURL(blob);
```

### Operations, configuration, and S3 keys

```ts
const health = await fg.system.health();
const runtime = await fg.system.runtime();
const sessions = await fg.system.uploadSessions({ phase: "in_progress" });
const pruned = await fg.system.prune();

const desired = { "metrics.enabled": true };
const plan = await fg.config.plan(desired);
await fg.config.apply(desired, plan.currentRevision);

const created = await fg.s3Keys.create({ buckets: ["data"] });
const rotated = await fg.s3Keys.rotate(created.accessKey);
await fg.s3Keys.delete(created.accessKey);
```

Plan before apply so the revision precondition protects concurrent changes.
`create` and `rotate` return the S3 secret exactly once.

## Error model — `FilegateError`

The non-`Raw` SDK methods throw `FilegateError` on non-2xx:

```ts
import { FilegateError } from "@k2b/filegate/client";

try {
  await fg.paths.put(path, body);
} catch (e) {
  if (e instanceof FilegateError) {
    console.log(e.status, e.message, e.method, e.path);

    // The raw response body text is always on `e.body`.
    // The parsed envelope (when valid JSON in the documented shape) is
    // on `e.errorResponse` — use that for typed conflict diagnostics:
    if (e.status === 409 && e.errorResponse) {
      console.log("collides with:", e.errorResponse.existingPath,
                  "(id:", e.errorResponse.existingId, ")");
    }
  } else {
    // network failure, fetch-level error, etc.
  }
}
```

Fields:

- `e.status: number` — HTTP status
- `e.method: string` — request method
- `e.path: string` — request path (relative)
- `e.body: string` — raw response body, may be empty
- `e.errorResponse?: ErrorResponse` — parsed `{ error, existingId?, existingPath? }`, present when the body was JSON in that shape

## Type exports for the discriminating

```ts
import type {
  Node, NodeListResponse,
  Ownership, OwnershipView,
  MkdirRequest, UpdateNodeRequest, TransferRequest, UploadSessionCreateRequest,
  GlobSearchResponse,
  ErrorResponse,
  FileConflictMode,    // "error" | "overwrite" | "rename"
  MkdirConflictMode,   // "error" | "skip" | "rename"
} from "@k2b/filegate/client";
```

## Conflict handling — quick reference

```ts
// File upload
await fg.paths.put(path, body, { onConflict: "error" });        // default — 409 if exists
await fg.paths.put(path, body, { onConflict: "overwrite" });    // replace existing
await fg.paths.put(path, body, { onConflict: "rename" });       // → "name-01.ext", etc.

// Mkdir
await fg.nodes.mkdir(parentId, { path: "x", onConflict: "error" });  // default
await fg.nodes.mkdir(parentId, { path: "x", onConflict: "skip" });   // idempotent
await fg.nodes.mkdir(parentId, { path: "x", onConflict: "rename" }); // → "x-01"
// "overwrite" is REJECTED for mkdir — use Transfer with overwrite to replace dirs.

// Upload sessions
await fg.uploads.sessions.create({ ..., onConflict: "error" });
await fg.uploads.sessions.create({ ..., onConflict: "overwrite" });

// Transfer
await fg.transfers.create({ ..., onConflict: "error" });
await fg.transfers.create({ ..., onConflict: "overwrite" });
await fg.transfers.create({ ..., onConflict: "rename" });
```

Full semantics in [`conflict-handling.md`](conflict-handling.md).
