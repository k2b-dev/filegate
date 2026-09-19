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
`{session,lease?}` for any file size, including zero. Import token-free browser
helpers from `@k2b/filegate/utils`: `DirectSession`, `putDirect`, `archiveRaw`,
`downloadArchive`, `segments` and `sha256`.

## Run with Unix permissions

`files.as(identity)` and `root.as(identity)` return independent backend clients
bound to an `ExecutionIdentity`; they do not change the original client.

```ts
import type { ExecutionIdentity } from "@k2b/filegate";
const identity: ExecutionIdentity = { uid: 10001, gid: 20001, groups: [20002] };
const actor = files.root("shared").as(identity);
const lease = await actor.directDownload("teams/editors/report.pdf");
```

The root must have `execution: true`. `uid` and `gid` are required; `groups` is
optional. Invalid IDs or more than 64 groups throw `TypeError`. Use the unscoped
client for root information, search and administrative operations. For an archive,
use `files.as(identity).archiveLease(items)`; the identity applies to every root.

Only backend requests carry `X-Filegate-Execution`. Return the lease unchanged
to the browser; direct helpers send neither this header nor the backend token.
Sessions retain their creation identity through renewal and commit. See
[Unix execution identity](/docs/en/permissions#unix-execution-identity) for
permissions, ownership overrides and failure semantics.

## Root operations

| Method | Result or action |
| --- | --- |
| `info()` | Root configuration, capabilities and dashboard metadata. |
| `stat(path)` | Current filesystem metadata; optional indexed identity. |
| `resolve(id)` | Current path for an indexed file ID. |
| `list(path, options?)` | Globally sorted/filtered page of immediate children. |
| `search(q, { path, after, limit, maxEntries, signal })` | Case-insensitive filename substring search. |
| `mkdir(path, { ownership, acl })` | Create one configured directory; parent must exist. |
| `setOwnership(path, ownership)` | Apply Unix ownership/mode without creating a version. |
| `getACL(path, scope)` | Read the access or default ACL. |
| `setACL(path, scope, acl)` | Replace one ACL; returns its stored entries. |
| `clearDefaultACL(path)` | Remove default inheritance from a directory. |
| `remove(path, recursive?)` | Permanent deletion including histories. |
| `transfer(path, targetRoot, targetPath, options)` | `TransferResult`; `move: true` selects a move. |
| `transferStatus(id)`, `resumeTransfer(id)`, `abandonTransfer(id)` | Inspect or reconcile a destination-root move receipt. |
| `copyVersion(path, id, targetRoot, targetPath, options?)` | Copy historical bytes to a distinct target; returns Node. |
| `directDownload(path, options?)` | GET/HEAD lease for the current file. |
| `directVersionDownload(path, id, options?)` | GET/HEAD lease for one historical version. |
| `directThumbnail(path, options?)` | GET/HEAD lease for a JPEG preview. |
| `createSession(path, size, options?)` | Create a session and its first lease. |
| `session(id)` | Read compact backend status and any recorded commit result. |
| `sessionSegments(id, after?, limit?)` | Ascending acknowledged `{index,hash}` receipts. |
| `sessionLease(id, { expiresIn, allowAbort })` | Issue a new lease for an open session. |
| `commitSession(id)` | Publish or return the original commit result. |
| `abortSession(id)` | Abort an open session; cannot remove a committed result. |
| `rebuild(signal?)` | Rebuild this root's metadata index. |
| `stats()` | Cached root totals or null. |
| `recursiveStats(path, maxEntries?, signal?)` | Subtree totals with completeness and freshness. |
| `refreshStats(maxEntries?, signal?)` | Refresh root cache; incomplete walks return 413. |
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

List and search accept `ListingOptions`: `sort`, `order`, `type`, `after`,
`limit` and `maxEntries`. Keep every option unchanged while following `next`,
including through empty indexed pages. Restart on `409 cursor_invalid`. See
[browsing](/docs/en/browsing) for sorting, query budgets and cursor lifetime.

Root streaming methods `contentRaw`, `thumbnailRaw` and
`versionContentRaw` do not throw on HTTP error responses. Typed JSON methods
throw `FilegateError` with `status`, `code` and `message`. Supply an optional
`fetch` in the constructor for testing or transport customization.

## Direct versions and previews

```ts
// Backend, after authorizing the path and version:
const version = await root.directVersionDownload("report.pdf", versionId, { expiresIn: 60 });
const thumbnail = await root.directThumbnail("photo.png", {
  width: 320, height: 180, expiresIn: 60,
});
```

Both methods return `Promise<DirectURL>` with `url`, `method: "GET"` and `expires`.
Current and historical downloads take `DownloadOptions` with optional `expiresIn`
and `fileName`. Thumbnail options do not accept a download filename.
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
if (!created.lease) throw new Error(`Session is already ${created.session.state}; reconcile its result`);
const transfer = new DirectSession(created.lease.url);
const progress = await transfer.upload(new Blob(["hello"]));
console.log(progress.state, progress.received);

// Backend, after checking the user's access and reserved budget again:
const published = await root.commitSession(created.session.id);
console.log(published.path, published.size);
const receipt = await root.session(created.session.id);
console.log(receipt.state, receipt.result);
```

`DirectSession` has `status`, `segments`, `put`, `upload` and `abort`; it cannot commit.
`upload` sends missing segments, checks already received segment hashes against
the supplied Blob, and returns transfer status. It does not publish. If a lease
expires, obtain another from your backend, construct a new `DirectSession` and
resume the same file. `abort` requires a lease issued with `allowAbort: true`.
Session options combine `WriteOptions` with `expiresIn`, `allowAbort` and optional
`idempotencyKey`. Creation returns an optional lease: a replay of a terminal
session returns its state/result without a new lease. The
session deadline remains 24 hours. Terminal receipts remain available for seven
days. See [direct transfers](/docs/en/uploads-downloads) for retry semantics.

## Internal server transfers

Set `transferBaseUrl` in `FilegateConfig` to an explicitly trusted internal HTTP(S)
origin. `files.downloadRaw(lease)`, `files.archiveRaw(lease)`,
`files.directSession(lease)` and `root.put` use it for server-side byte transfers.
`files.transferUrl(lease.url)` performs the same scoped-URL mapping when needed.
Issued URLs retain their public origin for browsers; transfer requests carry no
backend credentials. See [internal transfer origins](/docs/en/uploads-downloads#use-an-internal-transfer-origin).

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
buffer the archive in JavaScript. Fetch-based direct helpers set
`credentials: "omit"`; native form submission cannot omit target-origin cookies.
Use a dedicated Filegate origin outside the scope of application cookies. For server relays or custom streaming, use
`files.archiveRaw(lease, signal?)` or the token-free `archiveRaw(lease, options)`
utility. These return the HTTP response unchanged; check its status and stream
its body. `archiveLease(items, expiresIn?)` uses the same 60-second default and
300-second maximum as other leases. A selected directory includes its entire
current subtree. See the [archive limits](/docs/en/uploads-downloads#download-a-zip-selection).
