# @k2b/filegate

TypeScript client for Filegate. Use it from trusted server runtimes to call the
Filegate REST API, and from browsers only with scoped direct upload/download
URLs minted by your application server.

Filegate keeps files as normal Linux files on configured mounts. The client
covers path and ID lookup, direct URLs, upload sessions, transfers, search,
operations, declarative configuration, S3 credentials, activity, and stats.

## Install

```bash
npm i @k2b/filegate
```

## Server client

Use the default instance when `FILEGATE_URL` and `FILEGATE_TOKEN` are available
in the server runtime:

```ts
import { filegate } from "@k2b/filegate/client";

process.env.FILEGATE_URL = "http://127.0.0.1:8080";
process.env.FILEGATE_TOKEN = "dev-token";

const roots = await filegate.paths.get();
const caps = await filegate.capabilities.get();
```

Use an explicit instance for dependency injection:

```ts
import { Filegate } from "@k2b/filegate/client";

const fg = new Filegate({
  baseUrl: "https://filegate.internal.example",
  token: "<filegate-token>",
  fetchImpl: fetch,
});

const node = await fg.paths.get("photos/2026");
```

The client namespaces match the API surface:

| Namespace | Meaning |
| --- | --- |
| `paths` | Path-based lookup, upload, and directory listing. |
| `nodes` | ID-based metadata, content, mkdir, delete, and metadata edits. |
| `uploads` | One-shot direct upload URLs and resumable upload sessions. |
| `downloads` | Scoped direct download URLs. |
| `transfers` | Move and copy operations. |
| `search` | Indexed path search. |
| `index` | Index stats and rescan operations. |
| `stats` | Runtime, mount, cache, filesystem, and process stats. |
| `system` | Health, operational runtime, upload sessions, and version pruning. |
| `config` | Configuration schema, desired/effective values, manifest plan and apply. |
| `s3Keys` | S3 access-key creation, grants, rotation, and deletion. |
| `capabilities` | Server upload/download limits for adaptive clients. |
| `versions` | File version history on supported mounts. |
| `activity` | Recent API activity from the bounded server ring buffer. |

## Browser uploads

Do not expose the Filegate bearer token to browser code. The browser should ask
your application server for permission to upload. The application server then
creates direct Filegate upload URLs or direct upload sessions and returns the
scoped credentials to the browser.

```ts
import { upload } from "@k2b/filegate/client";

await upload({
  files,
  path: "photos/inbox",
  allow: async (request) => {
    const response = await fetch("/api/filegate/uploads/allow", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    });
    return response.json();
  },
  onEvent: (event) => {
    console.log(event.type, event);
  },
});
```

The high-level helper uses the transfer shape from the allow response. Small
files can use direct one-shot URLs. Larger files can use resumable sessions
with parallel segment uploads. Conflict handling is explicit; `skip-existing`
is the default.

For a single direct upload URL:

```ts
import { uploadDirect } from "@k2b/filegate/client";

await uploadDirect(uploadUrlFromYourServer, file, {
  onSuccess: ({ node }) => console.log(node.id),
});
```

## Browser downloads

Mint the direct download URL on your application server and redirect the
browser:

```ts
const direct = await fg.downloads.createDirectURL({
  nodeId: "<node-id>",
  expiresInSeconds: 5 * 60,
});

return Response.redirect(direct.downloadUrl, 303);
```

Direct file downloads support `GET`, `HEAD`, and byte ranges. Directory
downloads stream tar archives.

## Resumable sessions

Use upload sessions when you need resumable, parallel uploads with explicit
commit:

```ts
const session = await fg.uploads.sessions.create({
  path: "videos/input.mov",
  size,
  checksum,
  segmentSize: 16 * 1024 * 1024,
  onConflict: "error",
});

for (const segment of session.segments) {
  await fg.uploads.sessions.segments.put({
    sessionId: session.id,
    index: segment.index,
    body: segmentBody,
    checksum: segmentChecksum,
  });
}

const committed = await fg.uploads.sessions.commit({ sessionId: session.id });
console.log(committed.node.id);
```

Segment PUTs are idempotent when the content matches. Commit is explicit and
safe to retry after success.

## Utilities

Pure helpers live under `@k2b/filegate/utils` and do not require a
Filegate token:

```ts
import { uploads } from "@k2b/filegate/utils";

const checksum = await uploads.checksum.sha256(file);
```

## Documentation

- Project README: https://github.com/k2b-dev/filegate#readme
- TypeScript guide: https://filegate.dev/docs/en/ts-sdk
- HTTP routes: https://filegate.dev/docs/en/reference/http-routes
