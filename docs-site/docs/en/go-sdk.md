---
title: Go client
section: Integrate
order: 20
description: Use root operations and direct transfers from Go.
---

# Go client

Import `github.com/k2b-dev/filegate/v5/sdk/filegate`. The SDK works on platforms
other than Linux; only the daemon requires Linux.

```go
client, err := filegate.New("https://files.example.org", token)
if err != nil { return err }
root := client.Root("documents")
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
`DirectUpload(ctx, path, size, options, expiresIn)` and return its URL. Set
`expiresIn` to 0 for the 60-second default, or choose 1–300 seconds. The byte count
must match exactly. `filegate.PutDirect(ctx, url, reader, size)` sends no bearer token.

## Resumable upload

```go
created, err := root.CreateSession(ctx, "large.bin", size,
    filegate.WriteOptions{}, filegate.SessionLeaseRequest{ExpiresIn: 60})
if err != nil { return err }
session := filegate.DirectSession{URL: created.Lease.URL}
// Send each 8 MiB segment; the last contains the remaining bytes.
_, err = session.Put(ctx, 0, firstSegment)
if err != nil { return err }
status, err := session.Status(ctx)
if err != nil { return err }
_ = status.Segments
// Backend, after all segments and a fresh access/budget check:
node, err := root.CommitSession(ctx, created.Session.ID)
if err != nil { return err }
fmt.Println(node.Path, node.Size)
```

`DirectSession` needs only its scoped lease URL. `Status`, `Put` and `Abort`
all accept a context; there is no direct commit operation. Abort requires a lease
issued with `AllowAbort: true`. `Status` and `Put` return `SessionStatus`, which
omits backend options and the commit result. Use `root.Session(ctx, id)` for
backend status and `root.SessionLease(ctx, id, options)` to issue another lease.
Leases default to 60 seconds, maximum 300; sessions last 24 hours and retain
terminal results for seven days. The `segments` subpackage provides pure segment
arithmetic and checksums; `relay` provides streaming HTTP helpers.

## API map

The Go `Root` exposes `Info`, `Stat`, `Resolve`, `List`, `Search`, `Mkdir`,
`SetOwnership`, `GetACL`, `SetACL`, `ClearDefaultACL`, `Remove`, `Transfer`,
`DirectUpload`, `DirectDownload`,
`CreateSession`, `Session`, `SessionLease`, `CommitSession`, `AbortSession`,
`Rebuild`, `RefreshStats`, `Versions`, `Snapshot`,
`UpdateVersion`, `DeleteVersion`, `Restore` and `Prune`.
Each operation takes a `context.Context` first. Request structs shared with the
wire API live in `api/v1`, including `TransferRequest` and `VersionRequest`.

Root methods `ContentRaw`, `ThumbnailRaw` and `VersionContentRaw` return
`*http.Response` unchanged on HTTP errors. Always check `StatusCode` and close
the body. Typed operations return `*filegate.APIError` with `Status`, `Code` and
`Message`. Set caller deadlines through contexts; administrative rebuilds and
large transfers can take longer than a normal request.

## Permissions and ACLs

ACL methods take a context, a relative path and, for reads and writes, the scope
`"access"` or `"default"`:

```go
acl, err := root.SetACL(ctx, "teams/editors", "default", filegate.ACL{
    Entries: []filegate.ACLEntry{
        {Tag: "owner", Permissions: "rwx"},
        {Tag: "owningGroup", Permissions: "rwx"},
        {Tag: "other", Permissions: "---"},
    },
})
if err != nil { return err }
fmt.Println(acl.Entries)
current, err := root.GetACL(ctx, "teams/editors", "default")
if err != nil { return err }
fmt.Println(current.Entries)
```

`ClearDefaultACL(ctx, path)` removes the directory's default ACL. These operations
work without an index. See [permissions and ACLs](/docs/en/permissions) for setup,
inheritance and privilege requirements.

## Create a configured directory

```go
uid, gid := 10001, 20001
node, err := root.Mkdir(ctx, "teams/editors", filegate.DirectoryOptions{
    Ownership: &filegate.Ownership{UID: &uid, GID: &gid, DirMode: "2770"},
    ACL: &filegate.DirectoryACLs{Default: &filegate.ACL{
        Entries: []filegate.ACLEntry{
            {Tag: "owner", Permissions: "rwx"},
            {Tag: "owningGroup", Permissions: "rwx"},
            {Tag: "other", Permissions: "---"},
        },
    }},
})
if err != nil { return err }
fmt.Println(node.UID, node.GID, node.Mode)
```

The parent must already exist. Existing targets return 409 without changes.
Ownership and requested ACLs are applied before the directory becomes visible.

## Stream a ZIP selection

```go
lease, err := client.ArchiveLease(ctx, []filegate.ArchiveItem{
    {Root: "documents", Path: "reports", ArchivePath: "reports"},
    {Root: "shared", Path: "logo.png", ArchivePath: "logo.png"},
}, 60)
if err != nil { return err }
response, err := client.ArchiveRaw(ctx, lease)
if err != nil { return err }
defer response.Body.Close()
if response.StatusCode != http.StatusOK {
    return fmt.Errorf("archive download: %s", response.Status)
}
_, err = io.Copy(destination, response.Body)
return err
```

`ArchiveRaw` submits the manifest and returns the response unchanged. Stream the
body rather than buffering the archive. Authorize a selected directory's entire
subtree before requesting a lease. See [direct transfers](/docs/en/uploads-downloads)
for archive limits and transfer retry rules.
