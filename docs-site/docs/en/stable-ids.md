---
title: Stable file and directory IDs
section: Use
order: 10
description: Keep references across moves, read by ID and migrate indexed-only clients.
---

# Stable file and directory IDs

Store a **root name and ID** when your application needs to reference a file or
directory after its path changes. A root supports stable IDs only with both
`index: true` and `managed: true`. Read `stableIds` from `root.info()` in TypeScript
or `StableIDs` from `Root.Info(ctx)` in Go to check this capability. Read root
information with an unscoped backend client.

Set `managed` only when all content and namespace writes go through Filegate.
Keep it false for external NFS or local writers: it is an operator promise and
does not prevent those processes from changing files. Indexed unmanaged roots
retain their internal identities for versioning but do not expose `Node.id` or
`Version.fileId`, and reject public ID operations.

## Resolve a current path

Files and directories expose the same `Node.id` field. `root.resolve(id)` or
`Root.Resolve(ctx, id)` returns the current Node, including `path` and `directory`:

```http
GET /v1/roots/documents/resolve?id=019b72cf-5200-7000-8000-000000000001
Authorization: Bearer TOKEN
```

The lookup uses a per-root embedded Pebble key-value database, then checks the
live filesystem identity. It does not scan directories. A rebuild can delay
requests while it holds the root lock.

An ID is not an authorization grant. Your backend must authorize access to the
referenced object, and Unix execution scopes still enforce traversal and read
permissions. A path returned by `resolve` can change before a later path-based
request. Use a read by ID when the read must target that object.

## Read contents or issue a download by ID

For regular files, use `contentByIDRaw(fileId, signal?)` to stream from your
TypeScript backend, or `ContentByIDRaw(ctx, fileID)` in Go. Raw methods preserve
HTTP error responses; check the status and close the response body in Go.

To let an authorized browser download directly:

```ts
// Backend, after authorizing this root and stored file ID:
const root = files.root(reference.root);
const lease = await root.directDownloadByID(reference.id, { expiresIn: 60 });
// Return lease.url to the browser; keep the backend token private.
```

Go exposes `DirectDownloadByID(ctx, fileID, DownloadOptions)`. Both content reads
and lease issuance resolve the ID, open the file and verify that opened file's
identity under the same root lock. They cannot accidentally read a different
file placed at a former path between resolution and open. Directory IDs resolve
to Nodes; content and file-download operations require a regular file.

An ID-issued lease signs both the resolved path and the file ID. Each request
verifies the identity of the file opened at that path. Moving the file or
replacing it with a different identity makes that lease return `404 not_found`;
it does not follow a move or serve the replacement. Issue a fresh lease by ID
after a rename. Overwriting the same file retains its ID, so the lease can serve
its newer content. Use a version download when you need one concrete historical
content version.

The [HTTP API](/docs/en/http-api#stable-id-selectors) accepts `fileId` instead of
`path` for these two operations. Existing path-based reads, writes, thumbnails,
versions and transfers keep their path semantics. Downloads from either selector
support GET, HEAD and Range, with the usual 60-second default and 300-second
lease limit.

## Know when an identity changes

Filegate generates UUIDv7 IDs for new identities and stores each as a persistent
`user.filegate.id` extended attribute on the file or directory. The index can be rebuilt from these
attributes; restarting or rebuilding does not assign replacement IDs.

| Operation | Identity |
| --- | --- |
| Rename or move within one root | Retained, including every descendant of a moved directory. |
| Overwrite a file or restore one of its versions | Retained; the content revision changes. |
| Rebuild the index or restart the daemon | Retained. |
| Copy to a new file or directory | New IDs for the copy and its descendants. |
| Complete a cross-root move | Source IDs are not carried across; destination follows copy rules, then source identities and histories are removed. |
| Delete and create again at the same path | New ID. |

Copies, including cross-root moves, that explicitly overwrite an existing file
retain the destination's ID. A native same-root move onto an existing file keeps
the source ID and removes the replaced target's identity and history.
The ID identifies the file or directory; a managed file's `revision`/ETag
identifies its current content state.

Indexed roots need readable and writable user xattrs on their actual filesystem
and mount. Roots without an index do not need xattrs and do not offer stable IDs.
Copying an ID attribute onto another inode does not transfer the original identity
or history. For a full backup rollback, preserve inode identity, xattrs and the
complete state together. Ordinary file copies restored onto new inodes receive
new IDs even when they include the xattrs. See
[backup and recovery](/docs/en/operations#back-up-and-recover).

## Migrate clients that used indexed-only IDs

Public IDs now require managed writers as well as indexing. Before using
`Node.id`, `Version.fileId`, `resolve` or reads by ID, check the root's `stableIds`
capability instead of `index.enabled` alone. Treat both identity fields as
optional. Version IDs (`Version.id`) and path-based version operations remain
available when versioning is enabled on an unmanaged root.

Enable `managed: true` only after ensuring Filegate is the exclusive writer.
Otherwise retain path-based access and remove reliance on stable references for
that root. Changing this capability does not erase persisted identities or
histories; no index reset or ID migration is required.
