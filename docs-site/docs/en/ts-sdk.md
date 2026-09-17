---
title: TypeScript client
section: Integrate
order: 10
description: Use the backend client and scoped browser transfer helpers.
---

# TypeScript client

Install `@k2b/filegate` in your backend and pass the server URL and bearer token
to the client constructor.

```ts
import { Filegate, FilegateError } from "@k2b/filegate";
const files = new Filegate({
  baseUrl: process.env.FILEGATE_URL!,
  token: process.env.FILEGATE_TOKEN!,
});
const root = files.root("documents");
try {
  const file = await root.put("notes/today.txt", new Blob(["hello"]), {
    metadata: { message: "First note" },
  });
  console.log(file.path, file.id);
} catch (error) {
  if (error instanceof FilegateError && error.status === 409) {
    console.log("Choose another name or explicitly overwrite");
  } else throw error;
}
```

`put` mints a direct URL and uploads through it. `directUpload` returns the URL
instead, suitable for an authorized browser. `createSession` returns
`{session,lease}` for any file size, including zero. Import token-free browser
helpers from `@k2b/filegate/utils`: `DirectSession`, `putDirect`, `archiveRaw`,
`downloadArchive`, `segments` and `sha256`.

## Root operations

| Method | Result or action |
| --- | --- |
| `info()` | Root configuration, capabilities and dashboard metadata. |
| `stat(path)` | Current filesystem metadata; optional indexed identity. |
| `resolve(id)` | Current path for an indexed file ID. |
| `list(path, { after, limit })` | Alphabetical page of immediate children. |
| `search(q, { path, after, limit, maxEntries, signal })` | Case-insensitive filename substring search. |
| `mkdir(path, { ownership, acl })` | Create one configured directory; parent must exist. |
| `setOwnership(path, ownership)` | Apply Unix ownership/mode without creating a version. |
| `getACL(path, scope)` | Read the access or default ACL. |
| `setACL(path, scope, acl)` | Replace one ACL; returns its stored entries. |
| `clearDefaultACL(path)` | Remove default inheritance from a directory. |
| `remove(path, recursive?)` | Permanent deletion including histories. |
| `transfer(path, targetRoot, targetPath, options)` | Copy; set `move: true` for a move. |
| `directDownload(path, expiresIn?)` | GET/HEAD lease for the current file. |
| `directVersionDownload(path, id, expiresIn?)` | GET/HEAD lease for one historical version. |
| `directThumbnail(path, options?)` | GET/HEAD lease for a JPEG preview. |
| `createSession(path, size, options?)` | Create a session and its first lease. |
| `session(id)` | Read backend status and any recorded commit result. |
| `sessionLease(id, { expiresIn, allowAbort })` | Issue a new lease for an open session. |
| `commitSession(id)` | Publish or return the original commit result. |
| `abortSession(id)` | Abort an open session; cannot remove a committed result. |
| `rebuild(signal?)` | Rebuild this root's metadata index. |
| `refreshStats(maxEntries?, signal?)` | Explicit bounded filesystem accounting. |
| `versions(path)` | History, newest first. |
| `snapshot(path, { pinned, metadata })` | Immediate manual version. |
| `updateVersion(path, id, { pinned, metadata })` | Replace editable version attributes. |
| `restore(path, id)` | Restore content, preserving an undo snapshot. |
| `deleteVersion(path, id)` | Explicit version deletion, including pins. |
| `prune()` | Run retention now. |

Use `files.roots()` and `files.system()` for dashboards. Unknown recursive totals
are `null`; do not display them as zero. Indexed IDs persist through same-root
API moves. Use the root name and relative path for file operations. Indexed
roots also support resolving a file ID to its current path with `resolve(id)`.

List and search pages expose `next`. Pass it unchanged as `after` until absent.
Page limits are 1–1000. Search without an index returns 413 when its traversal
budget is exhausted. Narrow the search path or increase `maxEntries` to retry.

Root streaming methods `contentRaw`, `thumbnailRaw` and
`versionContentRaw` do not throw on HTTP error responses. Typed JSON methods
throw `FilegateError` with `status`, `code` and `message`. Supply an optional
`fetch` in the constructor for testing or transport customization.

## Direct versions and previews

```ts
// Backend, after authorizing the path and version:
const version = await root.directVersionDownload("report.pdf", versionId, 60);
const thumbnail = await root.directThumbnail("photo.png", {
  width: 320, height: 180, expiresIn: 60,
});
```

Both methods return `Promise<DirectURL>` with `url`, `method: "GET"` and `expires`.
The exported `ThumbnailLeaseOptions` has optional `width`, `height` and
`expiresIn`. Dimensions default to 256 and accept integers from 1 to 2048.
Expiry defaults to 60 seconds, maximum 300.

Pass the returned URL to the browser for `fetch(url)` or an image's `src` without
the backend token. Version downloads support Range; previews return a complete
JPEG. Leases bind their target and dimensions; a preview reads the current file
at its path. See [downloads and previews](/docs/en/uploads-downloads#downloads-and-previews)
for browser usage, expiry and missing-content behavior.

## Permissions and ACLs

Use `getACL(path, "access" | "default")`, `setACL(path, scope, acl)` and
`clearDefaultACL(path)`. The exported `ACL` and `ACLEntry` types describe numeric
identities and explicit permission strings. These methods work without an index.
See [permissions and ACLs](/docs/en/permissions) for a shared-directory setup and
[the HTTP ACL contract](/docs/en/http-api#posix-acls) for entry constraints.

## Upload with backend approval

```ts
// Backend:
const created = await root.createSession("inbox/unique-name.txt", 5, {
  onConflict: "error",
  expiresIn: 60,
  allowAbort: true,
});

// Browser, given only the lease:
import { DirectSession } from "@k2b/filegate/utils";
const transfer = new DirectSession(created.lease.url);
const progress = await transfer.upload(new Blob(["hello"]));
console.log(progress.state, progress.received);

// Backend, after checking the user's access and reserved budget again:
const published = await root.commitSession(created.session.id);
console.log(published.path, published.size);
const receipt = await root.session(created.session.id);
console.log(receipt.state, receipt.result);
```

`DirectSession` has `status`, `put`, `upload` and `abort`; it cannot commit.
`upload` sends missing segments, checks already received segment hashes against
the supplied Blob, and returns transfer status. It does not publish. If a lease
expires, obtain another from your backend, construct a new `DirectSession` and
resume the same file. `abort` requires a lease issued with `allowAbort: true`.
Session options combine `WriteOptions` with `expiresIn` and `allowAbort`; the
session deadline remains 24 hours. Terminal receipts remain available for seven
days. See [direct transfers](/docs/en/uploads-downloads) for retry semantics.

## Download a selection

```ts
// Backend, after authorizing every selection:
const lease = await files.archiveLease([
  { root: "documents", path: "reports", archivePath: "reports" },
  { root: "shared", path: "logo.png", archivePath: "logo.png" },
]);

// Browser, given the complete lease:
import { downloadArchive } from "@k2b/filegate/utils";
downloadArchive(lease);
```

`downloadArchive` submits a native form with the signed manifest; it does not
buffer the archive in JavaScript. For server relays or custom streaming, use
`files.archiveRaw(lease, signal?)` or the token-free `archiveRaw(lease, options)`
utility. These return the HTTP response unchanged; check its status and stream
its body. `archiveLease(items, expiresIn?)` uses the same 60-second default and
300-second maximum as other leases. A selected directory includes its entire
current subtree. See the [archive limits](/docs/en/uploads-downloads#download-a-zip-selection).
