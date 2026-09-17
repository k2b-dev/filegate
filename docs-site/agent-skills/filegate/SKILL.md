---
name: filegate
description: Integrate or operate Filegate, the Linux filesystem gateway. Use for the @k2b/filegate TypeScript client, Go SDK, root-scoped HTTP API, transfer leases, resumable sessions, ZIP selection downloads, version history, Unix ownership, setgid, POSIX ACLs, systemd deployment, static configuration, index rebuilds, dashboard metadata, backups or recovery. The application owns user authorization; Filegate serves independent named roots.
---

# Filegate

Use Filegate from a trusted application backend or administer its Linux daemon.
The public address is **root name + relative path**. Roots are independent and
cannot overlap. The application authenticates users and authorizes file access.

This self-contained skill covers the TypeScript API, Go API, HTTP contract and
operations below. It is also published by the Fibel agent-skills plugin.

## Contract

- The daemon requires Linux. Both SDKs are portable.
- One static YAML file and a separate token file; changes apply on restart.
- Index off means filesystem-based reads and no xattr requirement. Index on
  provides metadata search and stable IDs, updated by API writes or explicit
  rebuild. Versioning requires indexing.
- Stable IDs survive same-root API moves. Copies and cross-root moves start a
  new destination history. Permanent deletion removes history, including pins.
- Versions capture old contents before overwrite. Default cooldown is one minute;
  skipped writes do not restart it. Manual snapshots and restore bypass cooldown.
- Upload metadata belongs to the incoming revision. Versions expose arbitrary
  JSON-object metadata, limited to 8192 serialized UTF-8 bytes. Pins are exempt
  from automatic retention. History is not an immutable audit log.
- Numeric identities come from the trusted backend. New files use the daemon
  owner and inherit the group from a setgid parent unless UID/GID is explicit.
  Arbitrary chown requires OS privileges. Overwrites preserve existing ownership,
  ordinary permission bits and access ACL unless explicitly changed. Content
  replacement clears regular-file setuid/setgid bits, including on restore.
- POSIX access/default ACLs work independently of indexing. There is no NFSv4 ACL
  translation or recursive permission correction. Direct uploads and resumable
  commits enforce the ownership bound into their capabilities.

## Integration rules

1. Keep the full bearer token on trusted backends. Authorize the application user
   before issuing a scoped direct upload/download/session URL.
2. Use sessions for every file size when the backend must approve publication.
   Direct PUTs publish immediately. Both bind ownership, metadata and conflict
   policy at creation. Browser helpers never commit; recheck access and budget on
   the backend before committing.
3. Default conflict policy is `error`; choose `overwrite` or `rename` explicitly.
4. Use relative paths, never absolute server paths. Store `{root,id}` for indexed
   identity references, or `{root,path}` when indexing is off.
5. Raw stream methods preserve HTTP error responses. Check status and close Go
   response bodies. Never buffer a large relay into memory.
6. Do not delete `state_dir` to rebuild. Only the daemon owns live writable state;
   CLI rebuild/prune/stats calls its API.
7. A directory ZIP selection authorizes its entire current subtree. Do not use it
   to aggregate files with mixed access permissions.
8. Use the HTTP contract below and current API types to select routes and request
   fields when writing integration code.

When writing integration code, include construction, the relevant operation,
error handling and a read-back/status check. Operations examples are instructions
for the operator, not implicit permission to mutate a deployment.

## TypeScript API

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

### Root operations

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

### Permissions and ACLs

Use `getACL(path, "access" | "default")`, `setACL(path, scope, acl)` and
`clearDefaultACL(path)`. The exported `ACL` and `ACLEntry` types describe numeric
identities and explicit permission strings. These methods work without an index.
See [permissions and ACLs](https://filegate.dev/docs/en/permissions) for a shared-directory setup and
[the HTTP ACL contract](https://filegate.dev/docs/en/http-api#posix-acls) for entry constraints.


## Go API

Import `github.com/k2b-dev/filegate/v4/sdk/filegate`. The SDK works on platforms
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

### Resumable upload

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

### API map

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


## HTTP contract

All `/v1/*` routes require `Authorization: Bearer TOKEN`, except signed
`/v1/direct/{token}` routes. `GET /health` is unauthenticated. JSON errors have
`{"error":"code","message":"description"}`. Bodies use camelCase; configuration
uses snake_case. Unknown JSON fields are rejected.

Root operations have prefix `/v1/roots/{root}`. A `path` query parameter is a
relative path; `.` addresses the root where supported. Indexed roots also
provide file IDs. Use `GET /resolve?id=ID` to find a file's current path.

| Method and suffix | Input | Result |
| --- | --- | --- |
| `GET /v1/system` | — | Build, uptime, maintenance health. |
| `GET /v1/roots` | — | Root information array. |
| `GET /v1/roots/{root}` | — | Root information. |
| `GET /stat` | `path` | Node. |
| `GET /resolve` | `id` | Node, indexed roots only. |
| `GET /entries` | `path`, `after`, `limit` | `{items,next?}`. |
| `GET /search` | `q`, `path`, `after`, `limit`, `maxEntries` | Filename substring matches. |
| `GET /content` | `path` | File bytes, Range/HEAD supported. |
| `GET /thumbnail` | `path`, `width`, `height` | JPEG preview. |
| `POST /directories` | `{path,ownership?,acl?:{access?,default?}}` | Created Node; parent must exist. |
| `PATCH /ownership` | `path`; body `{uid?,gid?,mode?,dirMode?}` | Updated Node. |
| `GET /acl` | `path`, `scope=access\|default` | `{entries}`. |
| `PUT /acl` | `path`, `scope=access\|default`; body `{entries}` | Stored ACL. |
| `DELETE /acl` | `path`, `scope=default` | 204. |
| `DELETE /files` | `path`, `recursive` | 204. |
| `POST /transfers` | `{path,targetRoot,targetPath,move?,onConflict?,ownership?,metadata?}` | Destination Node. |
| `POST /uploads/direct` | `{path,size,expiresIn?,onConflict?,ownership?,metadata?}` | `{url,method,expires}`. |
| `POST /downloads/direct` | `{path,expiresIn?}` | `{url,method,expires}`. |
| `POST /uploads/sessions` | `{path,size,expiresIn?,allowAbort?,onConflict?,ownership?,metadata?}` | `{session,lease}`. |
| `GET /uploads/sessions/{id}` | — | Backend session status and optional commit result. |
| `POST /uploads/sessions/{id}/lease` | `{expiresIn?,allowAbort?}` | `{url,expires,operations}`. |
| `POST /uploads/sessions/{id}/commit` | — | Original committed Node. |
| `DELETE /uploads/sessions/{id}` | — | Abort; 204. |
| `GET /index` | — | Index status. |
| `POST /index/rebuild` | — | Final index status. |
| `GET /stats` | — | Cached recursive stats or null. |
| `POST /stats/refresh` | `maxEntries` | Recursive stats. |
| `GET /versions` | `path` | Versions, newest first. |
| `POST /versions` | `path`; body `{pinned?,metadata?}` | Manual version. |
| `PATCH /versions/{id}` | `path`; body `{pinned,metadata?}` | Updated version attributes. |
| `DELETE /versions/{id}` | `path` | 204. |
| `GET /versions/{id}/content` | `path` | Version bytes. |
| `POST /versions/{id}/restore` | `path` | Current Node. |
| `POST /versions/prune` | — | `{deleted}`. |

A Node contains `root`, `path`, optional `id`, `directory`, `size`, `modified`,
`mode`, `uid` and `gid`. Mode is an octal string including special bits, such as
`"2770"` for a setgid directory. Timestamps are RFC 3339, sizes are integer bytes. Directory
size is zero; recursive totals belong to stats. `limit` defaults to 100 and is
bounded by 1000. Search/stats traversal defaults to 100,000 entries and accepts
an explicit maximum of 10,000,000.

### POSIX ACLs

ACL routes require both `path` and `scope=access|default`. They also support `.`
for the root itself. Default ACLs apply only to directories. PUT replaces one
scope and returns its stored ACL; DELETE supports only `scope=default`.

```json
{
  "entries": [
    { "tag": "owner", "permissions": "rwx" },
    { "tag": "owningGroup", "permissions": "rwx" },
    { "tag": "group", "id": 20002, "permissions": "r-x" },
    { "tag": "mask", "permissions": "rwx" },
    { "tag": "other", "permissions": "---" }
  ]
}
```

Nonempty ACLs require exactly one `owner`, `owningGroup` and `other`. Named `user`
and `group` entries require a nonnegative numeric `id`, unique within the tag,
and an explicit `mask`. IDs range from 0 to 4294967294. PUT accepts 3–256 entries;
an empty array is invalid even for the default scope. Use DELETE to remove it.
Other tags do not accept `id`. Permission strings are
`rwx`, `rw-`, `r-x`, `r--`, `-wx`, `-w-`, `--x` or `---`.

Reading an access ACL returns at least its three base entries. Reading a default
ACL returns an empty entries array when none is present. Unsupported POSIX ACLs
return 501 (`acl_not_supported`). Invalid ACL input returns 400 (`invalid_acl`);
insufficient permissions return 403 (`forbidden`). Existing ACLs larger than
256 entries return 413 (`limit_exceeded`) when read, rather than being truncated.
ACLs work independently of indexing. They do not create versions or change
children recursively. See [permissions and ACLs](https://filegate.dev/docs/en/permissions) for mask
semantics, inheritance and shared-directory setup.

### Directory creation

`POST /directories` creates one new directory with optional ownership and access
and default ACLs. Each ACL is an `{entries}` object using the schema above. The
parent must exist. An existing target returns 409 without changing its metadata.
The target appears only after the requested permissions have been applied.
Staging and destination must share a filesystem; otherwise the operation returns
501 (`unsupported_storage_layout`). After a lost response, read the target state
before retrying. See [permissions](https://filegate.dev/docs/en/permissions) for an example.

### Transfer leases

`expiresIn` is an integer number of seconds, default 60, maximum 300. Zero also
selects the default. Expiry limits the start of new requests; accepted downloads
can continue. Leases are reusable and have no per-lease revocation. Upload write
options are bound at creation and cannot be changed through a lease.

Session creation returns separate `session` and `lease` objects. The session has
`id`, `root`, `path`, `size`, `chunkSize`, `expires`, `state`, `options`, `segments`
and `received`. Terminal sessions also expose `terminalAt`, `retainUntil` and,
when committed, the original Node in `result`. Sessions last 24 hours, independent
of their lease lifetime. Renewing a lease requires backend authentication and an
open session; it does not extend the session deadline.

Use the exact session lease URL:

| Method | Required lease operation | Meaning |
| --- | --- | --- |
| `GET URL` | `status` | Transfer state and received segment hashes. |
| `PUT URL?segment=N` | `write` | Exact segment bytes; session must remain open. |
| `DELETE URL` | `abort` | Abort the session. |

`status` and `write` are always granted. `abort` is granted only when the backend
sets `allowAbort: true` for that lease. A lease never permits commit or renewal.
Browser status omits the target path, write options and commit result.

Session states are `open`, `committed`, `aborted` and `expired`. Terminal records
are retained for seven days after completion or the session expiry time.
Repeated commits return the original Node, even if its path no longer exists.
Aborting a committed session returns `409 session_committed`; repeated aborts
return 204. Writes and commits on aborted sessions return `409 session_aborted`;
expired sessions return `410 session_expired`. A missing record after retention
returns 404, which does not reveal whether the upload committed.

### ZIP selection leases

`POST /v1/downloads/archives` requires backend authentication:

```json
{
  "items": [
    { "root": "documents", "path": "report.pdf", "archivePath": "report.pdf" },
    { "root": "shared", "path": "photos", "archivePath": "photos" }
  ],
  "expiresIn": 60
}
```

The response is `{url,method:"POST",expires,manifest}`. Submit the exact returned
`manifest` string to `url` as a single `manifest` field in an
`application/x-www-form-urlencoded` body. The manifest hash is signed; altered
selections are rejected. The response streams an uncompressed ZIP with attachment
headers. ZIP downloads do not support HEAD or Range.

Selections may span roots. A selected directory includes its whole current
subtree; the backend must authorize that scope. Private Filegate entries are
excluded from traversal, and explicit private selections are rejected. Symlinks,
unsafe archive paths and duplicate, nested or case-colliding selection names are
rejected. Limits: 1,000 selections, 128 KiB manifest, 10,000 expanded entries,
20,000 scanned objects (including excluded private entries), depth 64, 100 GiB of
file contents and four concurrent archive streams. Archive names must be valid
UTF-8 and portable Windows-compatible relative paths. Collision checks normalize
Unicode and ignore case. Use `path: "."` to select a root; an empty path is invalid.
Read failures after response headers abort the stream. Selections are not snapshots.

### Errors and retries

Raw stream routes return normal HTTP statuses. Common JSON statuses are 400 for
invalid input, 401 for authentication/lease failure, 403 for permissions,
404 for missing files or receipts, 409 for conflicts/closed sessions/disabled
features, 410 for expired sessions, 413 for limits, 501 for unsupported POSIX ACLs
or storage layout, and 503 for concurrent transfer capacity. Do not retry a mutation blindly after an
ambiguous transport failure: session commits are idempotent while their receipts
are retained; ordinary mutations require reading back the resulting state.

## Transfer examples

```ts
// Backend: authorize a unique inbox path and reserve the declared byte count.
const created = await root.createSession("inbox/unique.txt", 5, {
  onConflict: "error", expiresIn: 60, allowAbort: true,
});
// Browser: receive only the lease and application-level session reference.
import { DirectSession, downloadArchive } from "@k2b/filegate/utils";
const status = await new DirectSession(created.lease.url).upload(new Blob(["hello"]));
console.log(status.state, status.received);
// Backend: reauthorize and recheck budget before publishing.
const node = await root.commitSession(created.session.id);
console.log(node.path, node.size);
console.log(await root.session(created.session.id));

// Backend: authorize the whole selected subtree before requesting a ZIP lease.
const archive = await files.archiveLease([
  { root: "documents", path: "reports", archivePath: "reports" },
]);
// Browser: native form streams the response without a JavaScript Blob.
downloadArchive(archive);
```

After lease expiry, reauthorize on the backend and call
`root.sessionLease(id, { expiresIn: 60, allowAbort: true })`. Construct a new
`DirectSession` with that URL and resume the same file. Sessions accept empty files
and use 8 MiB segments, at most 10,000. Duplicate bytes are idempotent; changed
bytes for an existing segment return 409. `upload` checks already uploaded segment
hashes, sends missing segments and returns status without publishing.

Use global `files.archiveRaw(lease, signal?)` or token-free
`archiveRaw(lease, { fetch, signal })` for streaming HTTP relays. In Go use
`client.ArchiveLease(ctx, []filegate.ArchiveItem{...}, expiresIn)` and
`client.ArchiveRaw(ctx, lease)`. Check the response status and close its body.
Do not use `response.blob()` for large archives.

## Shared-directory setup

The example creates a directory owned by UID 10001 and GID 20001, writable by
that group. Use your own application's numeric IDs. Create it under an existing
parent whose permissions are already appropriate.

Create the directory and its permissions in one request. Its parent must already
exist. Filegate prepares the directory privately and publishes it only after the
requested ownership and ACLs are applied. An existing target returns 409 and is
not changed.

```ts
import type { ACL } from "@k2b/filegate";

const root = files.root("shared");
const path = "teams/editors";
const defaults: ACL = {
  entries: [
    { tag: "owner", permissions: "rwx" },
    { tag: "owningGroup", permissions: "rwx" },
    { tag: "other", permissions: "---" },
  ],
};

const directory = await root.mkdir(path, {
  ownership: { uid: 10001, gid: 20001, dirMode: "2770" },
  acl: { default: defaults },
});
const inherited = await root.getACL(path, "default");
console.log(directory.uid, directory.gid, directory.mode, inherited.entries);
```

Verify UID 10001, GID 20001, mode `2770` and the default entries before enabling
application access. The service needs permission to finish configuring the
directory after assigning its owner. You can also supply `acl.access` to set the
access ACL in the same request. An explicit `dirMode` sets the final effective
access permissions, including the ACL mask; named entries remain present. The
default ACL is independent of this mode.

Creation requires the destination and private staging area to share a filesystem.
An additional mount inside a root can prevent publication. After a lost response,
read the target state before retrying; a successful publication may already exist.

During setup, prevent external writers from renaming or replacing the parent or
changing its ownership, mode or ACLs. Inheritance uses the parent permissions read
when setup starts; Filegate does not coordinate these changes with NFS writers.

Without explicit overrides, new files inherit GID 20001 and group read/write
access. New subdirectories also inherit setgid and the default ACL. Ordinary
files do not inherit execute permission. An external program that explicitly
creates a file with `0600` can restrict inherited access: a default ACL provides
inheritance, not a mandatory minimum permission policy.

## Read and replace ACLs

An **access ACL** controls access to the current file or directory. A **default
ACL** is a directory template for future children; it does not grant access to
the directory itself. Changing or removing it does not update existing children.

`getACL(path, "access")` returns the owner, owning-group and other entries even
when no extended ACL is stored. `getACL(path, "default")` returns `{ entries: [] }`
when a supported directory has no default ACL. Unsupported ACLs return an error,
not an empty result.

`setACL` replaces exactly the requested scope. To add a named group, read the
current ACL, retain the entries you need and send the complete replacement:

```ts
await root.setACL("teams/editors", "access", {
  entries: [
    { tag: "owner", permissions: "rwx" },
    { tag: "owningGroup", permissions: "rwx" },
    { tag: "group", id: 20002, permissions: "r-x" },
    { tag: "mask", permissions: "rwx" },
    { tag: "other", permissions: "---" },
  ],
});
```

Every nonempty ACL needs one `owner`, `owningGroup` and `other` entry. Named `user`
and `group` entries require a numeric `id` and an explicit `mask`. The mask limits
all named entries and the owning group; Filegate does not widen it automatically.
PUT accepts 3–256 entries; IDs range from 0 to 4294967294. An empty PUT is invalid;
use `clearDefaultACL` to remove a default ACL. Permissions use exactly three characters: `rwx`, `rw-`, `r-x`, `r--`, `-wx`, `-w-`,
`--x` or `---`. IDs must be unique within each named entry type.

Use `clearDefaultACL(path)` to remove default inheritance. To remove extended
access entries, replace the access ACL with only owner, owning-group and other.
An explicit access-ACL replacement can change the mode reported by `stat`.
Likewise, setting `mode` or `dirMode` changes the ACL's effective permissions,
including its mask when present.

## Rights across file operations

| Operation | Rights |
| --- | --- |
| New upload or copied file | Inherits destination default ACL and setgid group; explicit ownership overrides apply. |
| New directory or parent | Inherits destination default ACL and setgid; explicit `dirMode` overrides the mode. |
| Overwrite or restore | Preserves ownership, ordinary permission bits and access ACL unless explicitly changed; clears file setuid/setgid bits. |
| Same-root move | Preserves the existing inode's ownership, mode and ACLs. |
| UID/GID-only change | Preserves existing access ACL and directory mode, including setgid. |
| Default-ACL change | Affects future children only. |

Direct uploads and resumable commits use these same rules. Without a default ACL,
new files default to 0644 and directories to 0755. With a default ACL, inherited file access is initially limited by 0666.
An explicit file mode is applied afterward and can widen or restrict effective
ACL permissions. Directories inherit using 0777, or an explicitly requested
`dirMode`; an explicit mode also sets their final access permissions. Existing
parent directories are not modified. No permission operation recursively
corrects existing files.

Modes are octal strings. File `mode` accepts permissions up to `0777`; `dirMode`
also supports setgid, for example `"2770"`. Read responses include special bits.
Use `dirMode` when calling `setOwnership` on a directory.


## Operations

Run one Linux daemon per writable root set and state directory. Use the packaged
systemd unit, or `filegate serve --config /etc/filegate/conf.yaml`.

```yaml
server:
  listen: "127.0.0.1:8080"
  public_url: "https://files.example.org"
  allowed_origins: ["https://app.example.org"]
auth:
  token_file: /etc/filegate/token
state_dir: /var/lib/filegate
uploads:
  max_file_size: 10GiB
roots:
  - name: documents
    path: /srv/filegate/documents
    index: true
    versioning:
      enabled: true
      cooldown: 1m
      keep: { last: 10, daily: 30, monthly: 12 }
  - name: shared
    path: /mnt/shared
    index: false
```

Root paths must exist and cannot overlap each other or state. Configuration is
strict YAML, applied on restart. The token file contains one random secret of
32–4096 bytes; no token is generated automatically. The root name binds its path
in durable state. Repointing a name to another directory is rejected.

### Commands

- `filegate validate`: check configuration, roots and token readability.
- `filegate status`: daemon build, uptime, readiness and maintenance errors.
- `filegate roots`: root/index/version/upload/filesystem dashboard metadata.
- `filegate rebuild ROOT`: rebuild derived index rows, pausing root mutations.
- `filegate stats ROOT`: explicitly scan recursive totals, bounded by 100,000 entries.
- `filegate prune ROOT`: apply configured retention immediately.

Administrative commands use `server.public_url` and bearer authentication. They
never open the live state database. The daemon prunes versions and expires upload
sessions every five minutes. Terminal receipts last seven days from completion
or the session deadline; a missing receipt afterward does not reveal whether an
upload committed. Session assembly temporarily needs roughly twice the upload
size, plus existing files, history and concurrent transfers. The application owns
aggregate storage budgets. There is no automatic scan for external file changes.

`keep` accepts last/hourly/daily/weekly/monthly counts. Retention keeps their union,
uses UTC calendar buckets including the current one, and excludes pins from tier
budgets. It selects existing snapshots; it does not schedule new ones. Omitting a
tier or using zero retains nothing for that tier.

### Permissions and systemd

The packaged service uses `filegate:filegate`. Create root storage for that user,
protect the token file, and allow root paths through systemd `ReadWritePaths`.
TLS belongs at the reverse proxy. Add exact browser origins for direct transfers.
Avoid logging signed URL paths.

Numeric UID/GID changes require privileges beyond an ordinary service account.
`CAP_CHOWN` alone does not allow reading arbitrary user-owned mode-0600 files.
Assess required privileges and NFS root-squash on the actual deployment. Metadata
operations require read access to targets and read/traverse access to directories;
retain service access when setting ACLs. Ownership and ACL changes can partially
succeed before a later step fails, so read back the state before retrying. Do not
change service identity, capabilities, mounts or production permissions without
the user's operational authorization.

### State and recovery

Keep the root namespace under daemon/operator control: external writers must not
be allowed to rename or replace `.filegate` or its lock file. Give them access to
their assigned subdirectories instead.

The private `.filegate` directory in each root stores version bytes and staging;
it must be daemon-owned with mode 0700. The separate `state_dir` stores both
rebuildable index rows and authoritative identities, revision metadata, versions,
sessions and recovery records. Never remove it as an index repair technique.

For a consistent full rollback, stop the daemon, coordinate external writers,
and snapshot all roots and state together, preserving ownership, modes, ACLs,
xattrs and inode identity.
Ordinary file-copy restore changes inode identity and is not a full history
restore: copied xattrs do not reconnect old histories. Current contents can be
imported as a new root/state; retain the original backup.

On restart, Filegate completes recorded publications/moves/deletes and cleans
unreferenced staging artifacts. An inconsistent recovery fails startup with an
error. Preserve the original state and logs before attempting repair.

Dashboard totals may be null until measured; do not display unknown as zero.
Version byte totals are logical, not allocated disk usage. Roots on the same
filesystem share capacity and must not be double-counted.

If the entire `keep` section is absent, defaults are `last: 10`, `daily: 30`,
and `monthly: 12`. An explicit `keep: {}` retains only pinned versions.
