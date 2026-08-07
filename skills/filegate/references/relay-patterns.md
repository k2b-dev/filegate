# Relay Patterns

A common architecture: the browser must not hold a Filegate token, so your backend sits between the browser and Filegate, doing per-user authorization and forwarding the actual bytes.

```
Browser (no Filegate token) ──HTTP──▶ Your backend ──HTTP+Bearer──▶ Filegate
```

The relay layer must NOT buffer file bodies — that defeats the streaming
property of `PUT /v1/paths` and `GET /v1/nodes/{id}/content`. Stream
end-to-end.

## The `*Raw` contract

For each operation that can be relayed, the SDKs offer two variants:

| Variant     | Examples                                          | On non-2xx       |
|-------------|---------------------------------------------------|------------------|
| **non-raw** | `paths.put`, `uploads.sessions.segments.put`, `nodes.delete`, `nodes.get`, `nodes.mkdir`, `transfers.create`, ... | **Throws** `FilegateError` (TS) / `*APIError` (Go). The body is parsed; conflict diagnostics are typed. |
| **`*Raw`**  | `paths.putRaw`, `nodes.contentRaw`, `nodes.thumbnailRaw`, `uploads.sessions.segments.putRaw` | Returns the raw `Response` / `*http.Response` **unchanged** — including 4xx/5xx. Caller owns the body. |

For relay/passthrough handlers always use the `*Raw` variant — the
upstream status, headers, and body all reach the downstream client
unchanged. The Go SDK pins this with
`TestRawMethodsDoNotThrowOnNon2xx`; the same property is smoke-tested
ad-hoc in TS.

## TS — relay handler examples

### Browser → Backend → Filegate (upload)

```ts
// Bun handler
import { filegate } from "@k2b/filegate/client";

app.put("/api/upload/:path{.+}", async (c) => {
  const userId = c.get("userId");                  // your auth
  const path = c.req.param("path");
  // Per-user authorization happens here

  const upstream = await filegate.paths.putRaw(
    `users/${userId}/${path}`,
    c.req.raw.body!,                               // ReadableStream — passed through
    {
      contentType: c.req.header("content-type") ?? "application/octet-stream",
      onConflict: c.req.query("onConflict") as any,
    },
  );
  // upstream is the raw Response — including 409/507/etc. Forward unchanged:
  return new Response(upstream.body, {
    status: upstream.status,
    headers: upstream.headers,
  });
});
```

**Node.js gotcha**: Node's built-in `fetch` (undici) requires
`duplex: "half"` when sending a `ReadableStream` body, otherwise it
throws `TypeError: RequestInit: duplex option is required when sending a body`.
The SDK calls `fetch` without that option, so streaming a Node request
body straight through hits this. Two ways to handle it:

```ts
// Option A: drop in a Node-aware fetchImpl when constructing the client
import { Filegate } from "@k2b/filegate/client";
const fg = new Filegate({
  baseUrl, token,
  fetchImpl: (url, init) => fetch(url, { ...init, duplex: "half" } as RequestInit),
});

// Option B: buffer once into a Buffer/Uint8Array before forwarding (fine
// for small bodies, defeats streaming for large ones)
const bytes = new Uint8Array(await c.req.arrayBuffer());
await fg.paths.putRaw(target, bytes, { contentType });
```

Bun and Deno don't have this restriction.

### Browser → Backend → Filegate (download)

```ts
app.get("/api/files/:id/content", async (c) => {
  const userId = c.get("userId");
  const id = c.req.param("id");
  // Authorization: confirm `id` belongs to `userId`

  const upstream = await filegate.nodes.contentRaw(id, { inline: c.req.query("inline") === "true" });
  return new Response(upstream.body, {
    status: upstream.status,
    headers: upstream.headers,                     // includes Content-Type, Content-Disposition
  });
});
```

The `Content-Disposition`, `Content-Type`, and `Content-Length` headers from
Filegate are preserved when the daemon set them.

**Note on Content-Length**: file downloads include it; directory tar
downloads do **not** (the tar size isn't known up front because it's
streamed on the fly). Don't assume it's always present.

## Go — the `relay` subpackage

```go
import (
    "github.com/k2b-dev/filegate/v3/sdk/filegate"
    "github.com/k2b-dev/filegate/v3/sdk/filegate/relay"
)

func downloadHandler(w http.ResponseWriter, r *http.Request) {
    userID := authUser(r)
    nodeID := chi.URLParam(r, "id")
    // Authorization: confirm nodeID belongs to userID

    resp, err := fg.Nodes.ContentRaw(r.Context(), nodeID, false /* inline */)
    if err != nil {
        // Network-level error only — ContentRaw does NOT throw on 4xx/5xx.
        http.Error(w, err.Error(), http.StatusBadGateway)
        return
    }
    // CopyResponse mirrors status + ALL headers + streams body.
    // Closes upstream Body when done.
    if _, err := relay.CopyResponse(w, resp); err != nil {
        log.Printf("relay copy failed: %v", err)
    }
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
    userID := authUser(r)
    path := mux.Vars(r)["path"]

    resp, err := fg.Paths.PutRaw(r.Context(),
        fmt.Sprintf("users/%s/%s", userID, path),
        r.Body,                                       // io.Reader — passed through
        filegate.PutPathOptions{
            ContentType: r.Header.Get("Content-Type"),
            OnConflict:  filegate.FileConflictMode(r.URL.Query().Get("onConflict")),
        })
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadGateway)
        return
    }
    relay.CopyResponse(w, resp)   // forwards status + body unchanged
}
```

## Why streaming matters

If you do this:

```ts
// ❌ DON'T
const buffer = await c.req.arrayBuffer();   // buffers whole file in memory
await fg.paths.put(path, buffer);
```

Then a 5 GB upload allocates 5 GB of RAM in your backend per concurrent
request. With the streaming pattern, memory usage is bounded by the HTTP
server's chunk buffer (typically tens of KB).

The same applies to downloads:

```ts
// ❌ DON'T
const blob = await (await fg.nodes.contentRaw(id)).blob();
return new Response(blob);                    // materializes the file in memory
```

vs.

```ts
// ✅ DO
const upstream = await fg.nodes.contentRaw(id);
return new Response(upstream.body, { status: upstream.status, headers: upstream.headers });
```

## Authorization patterns

The relay layer is where your per-user authorization happens — Filegate has
none of its own (single bearer token).

Common pattern: virtual paths under `users/<userId>/...`. The relay
enforces:

```ts
function checkPath(userId: string, path: string) {
  const expected = `users/${userId}/`;
  if (!path.startsWith(expected)) throw new HTTPException(403, "forbidden");
}
```

For ID-based access, maintain a `(node_id → owner_user_id)` mapping in your
own database. Resolve every request against it before forwarding.

## Upload sessions through a relay

For segment **PUTs**, the SDK's `*Raw` variants forward the upstream
response unchanged, including 409, 413, and 507 bodies:

```ts
const upstream = await filegate.uploads.sessions.segments.putRaw({
  sessionId: c.req.param("sessionId"),
  index: Number(c.req.param("index")),
  body: c.req.raw.body!,                            // streamed through
  checksum: c.req.header("X-Segment-Checksum"),
});
return new Response(upstream.body, {
  status: upstream.status,
  headers: upstream.headers,
});
```

For create/status/commit, the typed SDK methods are usually enough because
they exchange small JSON bodies. To relay a create `409` or `507` unchanged,
catch `FilegateError` and rebuild the JSON response:

```ts
// Option A: call the typed method, catch FilegateError, rebuild the
// response from its body/status (loses some response headers, fine for
// JSON bodies)
import { FilegateError } from "@k2b/filegate/client";
try {
  const result = await filegate.uploads.sessions.create(body);
  return Response.json(result);
} catch (e) {
  if (e instanceof FilegateError) {
    return new Response(e.body, {
      status: e.status,
      headers: { "Content-Type": "application/json" },
    });
  }
  throw e;
}

```

Server-side staging happens on Filegate's host, not in your backend — the
segments pass through your relay but are never buffered there. For browser
uploads that should bypass your backend's request-body path, create the
session with `direct: {}` on the backend and use `directUploads` in the
browser. For all other API calls, relay through your backend.
