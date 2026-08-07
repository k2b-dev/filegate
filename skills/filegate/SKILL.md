---
name: filegate
description: Integrate the Filegate file gateway into another application — uploading and downloading files, managing virtual paths, browsing nodes, generating thumbnails, running searches, doing resumable upload sessions, relaying streams from a browser through a backend, or wiring authentication. Use this skill whenever the user mentions "filegate" by name, asks about file storage with stable IDs, asks how to upload/download to filegate from a TypeScript/Node/Bun/browser app or a Go backend, mentions the @k2b/filegate npm package, or asks about REST endpoints under `/v1/paths`, `/v1/nodes`, `/v1/uploads/sessions`, `/v1/transfers`, or `/v1/search`. Also use this when integrating drag-and-drop uploads, building cloud relay handlers, debugging 409 conflicts, or implementing resumable file transfer flows. This skill is for code that USES Filegate from another project — for working ON Filegate itself, use the `filegate-dev` skill instead.
---

# Filegate

You are integrating Filegate into an application. Filegate is a Linux-only HTTP gateway that sits in front of a filesystem and gives the calling application:

- **Stable file IDs** (UUID v7, stored as a Linux xattr) that survive renames and moves
- **A Pebble-backed metadata index** for fast directory listing and path/id lookup
- **Resumable, duplicate-safe upload sessions** with explicit commit
- **Tar-stream downloads** of whole subtrees
- **On-demand thumbnails** with LRU caching for images
- **Per-file versioning** with probed reflinks or explicit byte-copy fallback
- **Glob-based search** scoped to virtual mounts
- **Automatic external change detection** (btrfs find-new or polling)
- **Bearer-token authentication** on every `/v1/*` endpoint

## What Filegate can do — pick the right tool

| Task                                              | Read first                                                          |
|---------------------------------------------------|---------------------------------------------------------------------|
| First time integrating, want the full picture     | [`references/function-overview.md`](references/function-overview.md) |
| Decide which client (TS / Go / raw HTTP)          | [`references/clients.md`](references/clients.md)                    |
| One-shot upload at a virtual path (small files)   | [`references/ts-sdk.md`](references/ts-sdk.md) or [`references/go-sdk.md`](references/go-sdk.md) |
| Large/resumable uploads with progress             | [`references/upload-sessions.md`](references/upload-sessions.md)    |
| Browser → my backend → Filegate (streaming relay) | [`references/relay-patterns.md`](references/relay-patterns.md)      |
| Direct browser upload URL from a backend          | [`references/ts-sdk.md`](references/ts-sdk.md) and [`references/http-api.md`](references/http-api.md) |
| User uploads a file with a name that already exists | [`references/conflict-handling.md`](references/conflict-handling.md) |
| Build a thumbnail gallery / glob search           | [`references/function-overview.md`](references/function-overview.md) (sections "Thumbnails" + "Search") |
| Wire up auth and handle errors                    | [`references/auth-and-errors.md`](references/auth-and-errors.md)    |
| Decide production fit, deploy, back up, or restore | [`references/production-deployment.md`](references/production-deployment.md) |
| Raw HTTP — no SDK                                 | [`references/http-api.md`](references/http-api.md)                  |
| Something is broken, weird status code, slow      | [`references/troubleshooting.md`](references/troubleshooting.md)    |

## Hard rules — non-negotiable

- **All `/v1/*` requests need `Authorization: Bearer <token>` except
  scoped direct upload and download URLs.** The other auth-free endpoint is
  `GET /health`. An empty bootstrap token generates and persists a token; it
  never enables unauthenticated REST.
- **Use the right client construction for your runtime.** Server (Node/Bun)
  → env-based default. **Public browser apps must NOT construct a
  `Filegate` client at all** — Filegate's bearer token must never reach an
  end-user's browser; route through a backend relay
  ([`references/relay-patterns.md`](references/relay-patterns.md)).
  Explicit `new Filegate({ baseUrl, token })` is for server-side dependency
  injection, tests, trusted internal tooling, or non-public/internal
  browser environments where the token is already trusted by the runtime.
  Details in [`references/ts-sdk.md`](references/ts-sdk.md).
- **Never assume an upload silently overwrites an existing file.** Default is `onConflict: "error"` — a 409 is returned with diagnostic fields. Choose `overwrite`, `rename`, or `skip` (mkdir only) explicitly. See [`references/conflict-handling.md`](references/conflict-handling.md).
- **Never bundle the full TS client just to compute a sha256 or segment bounds.** Pure helpers ship as a tree-shakeable subpackage `@k2b/filegate/utils` (Go: `sdk/filegate/segments` + `sdk/filegate/relay`). Use those — no token needed.
- **Stream relays must not buffer.** When proxying browser uploads/downloads through your backend, pass through `ReadableStream` / `io.Reader` end-to-end. See [`references/relay-patterns.md`](references/relay-patterns.md).
- **Persist file IDs, not paths.** Filegate's `id` is stable across renames/moves; the virtual `path` is not. If you store something in your own database that points to a Filegate node, store the `id`.
- **Treat external filesystem changes as eventually consistent.** If something else writes to a mount, Filegate's index converges via the change detector — but not instantly. Don't race against the detector in tests; use `POST /v1/index/rescan` to force convergence when you need it.
- **Do not design Filegate as a shared or replicated daemon.** Run one active
  process per runtime/index store and writable mount set. Preserve data xattrs
  and the separate runtime config store in backups. See
  [`references/production-deployment.md`](references/production-deployment.md).

## Required output pattern

When you generate integration code:

1. Show client construction matched to the target runtime.
2. Show the operation itself (paths/nodes/uploads/...).
3. Show error handling that includes status, message, and the conflict diagnostic fields where relevant.
4. Show a verification snippet (a `curl` or a quick read-back) the user can run to confirm.

Don't invent endpoint names or fields that aren't in the API contract — when unsure, point to [`references/http-api.md`](references/http-api.md).
