# Choosing a Client

Filegate ships three integration paths. Pick based on your runtime:

| Runtime                                 | Use                                                                  |
|-----------------------------------------|----------------------------------------------------------------------|
| Node / Bun / Deno (server-side)         | TS SDK — [`ts-sdk.md`](ts-sdk.md)                                    |
| Browser (trusted internal only)         | TS SDK with explicit construction — [`ts-sdk.md`](ts-sdk.md). For public browser apps, **do NOT** construct the client; relay through your backend ([`relay-patterns.md`](relay-patterns.md)) or use direct upload sessions; import only `@k2b/filegate/utils` for hashing/segment math. |
| Go service                              | Go SDK — [`go-sdk.md`](go-sdk.md)                                    |
| Anything else (Python, Rust, curl, ...) | Raw HTTP — [`http-api.md`](http-api.md)                              |
| Just need sha256/segment math, no I/O  | Tree-shakeable subpackage — see "Pure helpers" below                 |

## TS vs Go — they expose the same shape

Both SDKs provide the same scoped namespaces:

```
client.paths      ← virtual path operations (one-shot PUT, GET listings/metadata)
client.nodes      ← ID-based operations (get, content, mkdir, patch, delete, thumbnail)
client.uploads    ← upload sessions (.sessions.create/status/segments.put/commit/abort)
client.downloads  ← direct download URL minting
client.transfers  ← move / copy
client.search     ← glob search
client.index      ← rescan, resolve
client.stats      ← daemon stats
client.versions   ← per-file version history
```

One-shot uploads live under `client.paths.put` — not under `client.uploads`.
`client.uploads` contains resumable upload sessions and direct upload URL minting.

Method names differ in case (`paths.put` vs `Paths.Put`) but the semantics are identical. If you know one, you can use the other.

## Pure helpers — when you don't need a client

Both SDKs split off pure, non-network helpers into dedicated subpackages so callers without a token (browsers doing client-side hashing, build tools, tests) can use them without bundling the HTTP client.

**TypeScript:**

```ts
import { uploads } from "@k2b/filegate/utils";

await uploads.checksum.sha256(uint8Array);
uploads.segments.count({ size: fileSize, segmentSize });
uploads.segments.bounds(index, fileSize, segmentSize);
```

The `/utils` subpath strips the entire HTTP client from the bundle — typically <1 KiB minified.

**Go:**

```go
import (
    "github.com/k2b-dev/filegate/v3/sdk/filegate/segments"
    "github.com/k2b-dev/filegate/v3/sdk/filegate/relay"
)

sum := segments.SHA256Bytes(data)
total := segments.Count(size, segmentSize)
start, end, err := segments.Bounds(index, size, segmentSize)

// HTTP relay (proxying upstream Filegate response to your own ResponseWriter)
n, err := relay.CopyResponse(w, upstreamResp)
```

## Raw HTTP — when neither SDK fits

Every endpoint is documented in [`http-api.md`](http-api.md). Three rules:

1. Always send `Authorization: Bearer <token>`.
2. JSON request bodies use `Content-Type: application/json`. File bodies use the actual MIME type (or `application/octet-stream`).
3. Errors come back as `{ "error": "..." }`. On 409, also `{ "error": "...", "existingId": "...", "existingPath": "..." }`.

## SDK installation

**TypeScript:**

```bash
npm i @k2b/filegate
# or yarn / pnpm / bun
```

**Go:**

```bash
go get github.com/k2b-dev/filegate/v3/sdk/filegate
go get github.com/k2b-dev/filegate/v3/sdk/filegate/segments # if you need pure helpers
go get github.com/k2b-dev/filegate/v3/sdk/filegate/relay    # if you need the relay helper
```

## Backend + Browser? Use the relay pattern

A common architecture:

```
Browser (untrusted, no Filegate token)
    ↓ HTTP
Your backend (Node/Bun/Go, holds the Filegate token)
    ↓ HTTP (Bearer)
Filegate
```

The TS SDK has explicit `paths.putRaw` / `nodes.contentRaw` methods that return the raw `Response` object so you can pass through `body`, `headers`, and `status` without buffering. The Go SDK has the `relay` subpackage. Full patterns in [`relay-patterns.md`](relay-patterns.md).
