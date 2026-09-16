---
title: Go client
section: Integrate
order: 20
description: Use root operations and direct transfers from Go.
---

# Go client

Import `github.com/k2b-dev/filegate/v3/sdk/filegate`. The SDK works on platforms
other than Linux; only the daemon requires Linux.

```go
client, err := filegate.New("https://files.example.org", token)
if err != nil { return err }
root := client.Root("cloud")
node, err := root.Put(ctx, "notes/today.txt", strings.NewReader("hello"), 5,
    filegate.WriteOptions{Metadata: filegate.Metadata{"message": "First note"}})
if err != nil {
    var apiErr *filegate.APIError
    if errors.As(err, &apiErr) && apiErr.Status == 409 {
        // Ask the application user to choose a conflict policy.
    }
    return err
}
fmt.Println(node.Path, node.ID)
```

`Put` uses a scoped direct upload. To hand the transfer to another client, call
`DirectUpload(ctx, path, size, options)` and return its URL. The byte count must
match exactly. `filegate.PutDirect(ctx, url, reader, size)` sends no bearer token.

## Resumable upload

```go
created, err := root.CreateSession(ctx, "large.bin", size, filegate.WriteOptions{})
if err != nil { return err }
session := filegate.DirectSession{URL: created.URL}
// Send each 8 MiB segment; the last contains the remaining bytes.
_, err = session.Put(ctx, 0, firstSegment)
if err != nil { return err }
status, err := session.Status(ctx)
if err != nil { return err }
_ = status.Segments
// After all segments:
node, err := session.Commit(ctx)
```

`DirectSession` needs only its scoped URL. `Status`, `Put`, `Commit` and `Abort`
all accept a context. The `segments` subpackage provides pure segment arithmetic
and checksums; `relay` provides streaming HTTP helpers.

## API map

The Go `Root` exposes `Info`, `Stat`, `Resolve`, `List`, `Search`, `Mkdir`,
`SetOwnership`, `Remove`, `Transfer`, `DirectUpload`, `DirectDownload`,
`CreateSession`, `Rebuild`, `RefreshStats`, `Versions`, `Snapshot`,
`UpdateVersion`, `DeleteVersion`, `Restore` and `Prune`.
Each operation takes a `context.Context` first. Request structs shared with the
wire API live in `api/v1`, including `TransferRequest` and `VersionRequest`.

`ContentRaw`, `ArchiveRaw`, `ThumbnailRaw` and `VersionContentRaw` return
`*http.Response` unchanged on HTTP errors. Always check `StatusCode` and close
the body. Typed operations return `*filegate.APIError` with `Status`, `Code` and
`Message`. Set caller deadlines through contexts; administrative rebuilds and
large transfers can take longer than a normal request.
