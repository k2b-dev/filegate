---
name: filegate
description: Integrate or operate Filegate, the Linux filesystem gateway. Use for @k2b/filegate, the Go SDK, root-scoped HTTP API, Unix execution identities and ACLs, direct leases, resumable sessions, managed revisions, browsing, historical copies, recoverable transfers, systemd configuration or backups. The application owns identities and authorization.
---

# Filegate

Use Filegate from a trusted backend or operate its Linux daemon. Address files by
**root name + relative path**. Root trees cannot overlap. The application owns
users, groups, shares, revocation, expiry, quotas and authorization.

Keep the bearer token on the backend. Authorize each operation before issuing a
short-lived lease. Browsers transfer bytes directly with that lease and never
receive the token. Filegate has no Cloud identity lookup or public-share model.

## Root capabilities

Read `root.info()` / `Root.Info(ctx)` with the unscoped client. Capabilities are
per root and independent except where noted:

- `index`: filename search and stable xattr file IDs, updated by API writes or
  explicit rebuild. External changes require a rebuild. Without an index, no
  xattr support is required.
- `versioning.enabled`: requires indexing; captures previous contents before
  overwrite. Versions are not an immutable audit log.
- `managed`: the operator promises exclusive Filegate content/namespace writers.
  Enables conditional publication and recoverable cross-root moves. Keep false
  for external NFS/local writers; it does not lock those writers out.
- `execution`: accepts backend-bound numeric Unix credentials. Requires explicit
  Linux root-service configuration; default false.

Do not infer that indexed data is current, missing totals are zero, or a file ID
is an authorization grant. Same-root moves preserve ID/history; new copies and
cross-root moves start a new destination identity/history.

## TypeScript API

```ts
import { Filegate, FilegateError } from "@k2b/filegate";
const files = new Filegate({
  baseUrl: process.env.FILEGATE_URL!,
  token: process.env.FILEGATE_TOKEN!,
  // Optional, explicitly trusted origin for this backend's own transfers:
  // transferBaseUrl: "http://filegate.internal:8080",
});
const root = files.root("documents");
const upload = await root.directUpload("notes.txt", 5, { onConflict: "error" });
// Return upload.url to the authorized browser for a five-byte PUT.
```

`root.put(path, Blob, WriteOptions, signal?)` issues a direct lease and uploads.
`WriteOptions` has optional `onConflict`, `ownership`, `accessACL`, `metadata` and
`precondition`. `FilegateError` exposes `status`, `code`, `message`.

| Method | Contract |
| --- | --- |
| `info()`, `stat(path)`, `resolve(id)` | Root capabilities or current Node. |
| `list(path?, ListingOptions?)` | Immediate children, sorted/filtered before paging. |
| `search(q, ListingOptions & {path?,signal?})` | Descendant filename matches. |
| `mkdir(path, {ownership?,acl?:{access?,default?}})` | Prepare permissions, then publish one directory; parent must exist. |
| `setOwnership`, `getACL`, `setACL`, `clearDefaultACL` | Explicit live metadata changes; no recursive correction or versions. |
| `remove(path, recursive?)` | Permanent removal, including histories. |
| `transfer(path,targetRoot,targetPath,TransferOptions?)` | `TransferResult`; inspect state before considering a move complete. |
| `transferStatus(id)`, `resumeTransfer(id)`, `abandonTransfer(id)` | Destination-root move receipts. |
| `copyVersion(path,id,targetRoot,targetPath,WriteOptions?)` | Historical bytes at a distinct target; returns Node. |
| `directDownload(path, DownloadOptions?)` | GET/HEAD lease; Range supported. |
| `directVersionDownload(path,id,DownloadOptions?)` | One concrete version, current path/file identity. |
| `directThumbnail(path,{width?,height?,expiresIn?})` | Bound JPEG preview lease. |
| `createSession(path,size,SessionCreateOptions?)` | `{session,lease?}`; lease absent on terminal replay. |
| `session(id)`, `sessionSegments(id,after?,limit?)` | Compact state and separate paged chunk receipts. |
| `sessionLease(id,{expiresIn?,allowAbort?})` | Renew access to an open session, never extend its deadline. |
| `commitSession(id)`, `abortSession(id)` | Backend-only publication or abort. |
| `stats()`, `recursiveStats(path,maxEntries?,signal?)` | Cached totals or bounded subtree observation. |
| `refreshStats(maxEntries?,signal?)`, `rebuild(signal?)`, `prune()` | Explicit maintenance. |
| `versions`, `snapshot`, `updateVersion`, `restore`, `deleteVersion` | File version history. |

`DownloadOptions` has `expiresIn?` and `fileName?`; expiry is not a positional
number. Raw `contentRaw`, `thumbnailRaw` and `versionContentRaw` preserve HTTP
error responses: inspect status. `files.roots()` and `files.system()` are
administrative dashboard methods.

`files.as(identity)` and `root.as(identity)` create independent execution scopes;
the original client stays unchanged. `ExecutionIdentity` is
`{uid:number,gid:number,groups?:number[]}`.

For internal byte transfers, `downloadRaw(lease)`, `archiveRaw(lease)`,
`directSession(lease)` and `root.put` use configured `transferBaseUrl`.
`transferUrl(lease.url)` maps an issued signed lease explicitly. Returned lease
URLs retain the public origin. Direct requests send no bearer or execution
header. This is not an arbitrary URL import facility.

## Go API

Import `github.com/k2b-dev/filegate/v5/sdk/filegate`; the SDK is portable, the
daemon Linux-only. HTTP request envelopes also live in `api/v1`.

```go
client, err := filegate.New("https://files.example.org", token)
if err != nil { return err }
root := client.Root("documents")
lease, err := root.DirectDownload(ctx, "report.pdf", filegate.DownloadOptions{
    ExpiresIn: 60, FileName: "Approved report.pdf",
})
if err != nil { return err }
response, err := client.DownloadRaw(ctx, lease)
if err != nil { return err }
defer response.Body.Close()
if response.StatusCode != http.StatusOK {
    return fmt.Errorf("download: %s", response.Status)
}
_, err = io.Copy(destination, response.Body)
return err
```

Typed errors are `*filegate.APIError` with `Status`, `Code`, `Message`. Raw methods
return `*http.Response` unchanged; check status and close the body. Contexts set
caller deadlines.

- `List(ctx,path,ListingOptions)` and `Search(ctx,q,path,ListingOptions)` use the
  same options/cursors as TypeScript.
- `DirectUpload(ctx,path,size,WriteOptions,expiresIn)` and `Put` issue direct PUTs.
- `DirectDownload(ctx,path,DownloadOptions)` and
  `DirectVersionDownload(ctx,path,versionID,DownloadOptions)` use typed options.
- `DirectThumbnail(ctx,path,width,height,expiresIn)` requires dimensions; use
  256,256 for default bounds.
- `CreateSession(ctx,path,size,WriteOptions,SessionCreateOptions)` returns an
  optional `Lease`. Check it before dereferencing; a terminal retry has no lease.
- `Session`, `SessionSegments(ctx,id,after,limit)`, `SessionLease`, `CommitSession`
  and `AbortSession` mirror backend session operations.
- `DirectSession{URL: lease.URL}` exposes `Status`, `Segments`, `Put`, `Abort`, all
  with context first; no commit. `filegate.PutDirect` sends no backend token.
- `Transfer(ctx,api.TransferRequest)` returns `TransferResult`;
  `TransferStatus`, `ResumeTransfer`, `AbandonTransfer` take `(ctx,id)` on the
  destination root.
- `CopyVersion(ctx,versionID,api.VersionCopyRequest)` returns the destination Node.
- `Stats`, `RecursiveStats(ctx,path,maxEntries)` and `RefreshStats` distinguish
  cached, subtree and root observations.
- `SetOwnership`, `GetACL`, `SetACL`, `ClearDefaultACL`, `Mkdir`, `Versions`,
  `Snapshot`, `UpdateVersion`, `Restore`, `DeleteVersion`, `Rebuild`, `Prune`
  expose their corresponding root operations.
- `Client.WithExecution(identity)` / `Root.WithExecution(identity)` return a new
  client and error. Identity fields: `UID uint32`, `GID uint32`, `Groups []uint32`.
- `Client.WithTransferBaseURL(origin)` returns a new client and error.
  `DownloadRaw`, `ArchiveRaw`, `DirectSession` and `Root.Put` use that origin;
  `TransferURL(lease.URL)` maps a signed lease explicitly.

## Publication and revisions

Default conflict policy is `error`. `rename` chooses a random sibling with a
128-bit suffix and a bounded retry count; always use the returned path.
`overwrite` supports regular-file-to-regular-file replacement, never directory
merge. A new destination gets a new ID/history; file overwrite retains the
existing destination identity and snapshots its old content per policy.

On managed roots, current stat/content exposes an opaque `revision`/ETag for
regular files. Listing revisions may be absent or stale; fetch stat or current
content before selecting an If-Match condition. Use
`precondition: {ifMatch: node.revision}` with overwrite, or
`{ifNoneMatch:true}` for create-only. Choose exactly one condition; conditions
cannot rename, and ifNoneMatch cannot overwrite. Preconditions are bound when
issuing a direct lease or creating a session.

Mismatch returns `412 precondition_failed` before target mutation, parent
creation, identity assignment or history capture. A session retains its segments
and stays open; changing its condition requires a new session. Unmanaged roots
reject conditions with `409 feature_disabled`.

Optional publishing `If-Match`/`If-None-Match` headers must exactly repeat the bound
condition (quoted revision or `*`); unbound/different headers return
`400 precondition_header_mismatch`. Omitting the headers does not disable the
condition. Stat/current-content/publication expose quoted ETags when available.

Atomicity covers Filegate operations on exclusive-writer roots. Live inode,
size, nanosecond mtime and ctime detect observed external changes, but cannot make
uncooperative NFS changes atomic. Metadata changes may invalidate revisions too.
Never describe this as external-writer CAS.

## Sessions and recovery

Sessions last 24 hours; segments are 8 MiB, last segment shorter, maximum 10000.
Zero-byte sessions need no segments. The backend must commit; browser helpers
cannot commit or renew. Cloud/application reservations remain separate.

`SessionCreateOptions` adds `expiresIn`, `allowAbort` and optional `idempotencyKey`.
Generate a key per logical upload and persist it before issuance. Keys are
root-scoped strings <=128 bytes without leading/trailing whitespace. Retrying
identical path/size/write options/execution with the same key returns the same
session while retrievable. Lease duration/abort permission may differ; changing
the bound upload returns 409. A terminal replay omits the lease.

Status contains `uploadedSegments` and `received`, not the full hash map. Use
paged `{items:[{index,hash}],next?}` receipts, start after=-1, limit 1–1000
(default 100), then follow `next`. Terminal pages are empty. Browser routes:

- `GET leaseURL`: compact status.
- `GET leaseURL?segments=1&after=-1&limit=100`: receipts.
- `PUT leaseURL?segment=N`: exact segment bytes.
- `DELETE leaseURL`: abort only if enabled.

Repeating identical segment bytes is safe; different bytes at an accepted index
return 409. Commit verifies lengths/hashes and returns the original Node on retry,
even after rename/deletion/restart. Concurrent commit/abort has one terminal
outcome. Aborting committed returns 409; repeated abort is safe. Terminal receipts
last seven days from terminal transition (expiry uses the session deadline).
After retention, 404 does not prove failure. Idempotency mappings last while their
session/receipt remains available.

Browser `DirectSession` from `@k2b/filegate/utils` binds native fetch correctly.
`upload` checks acknowledged hashes once and sends missing segments. Local
AbortSignal cancellation does not abort server state. GET and replayable PUT may
retry twice for transport errors or 429/502/503/504, with waits at most 2 seconds;
longer Retry-After returns control rather than retrying early. No automatic
retry of abort, non-replayable bodies or expired leases.

## Transfers and history

`TransferOptions` adds `move?` and `id?` to WriteOptions. Copies and same-root moves
return `{state:"completed",node}`. Same-root moves preserve the inode/ID/history
and accept only onConflict; other write options are rejected. File-over-file
native move removes the replaced target's identity/history. Source==target with
rename selects a sibling; ordinary same-path moves conflict.

Directory copies stage privately and publish the complete tree. Created ancestors
may remain after failure, but no partial copied subtree becomes visible. Tree
copies are bounded to 100000 entries and depth 128. A target inside the source
directory is invalid. Historical copies require a
distinct target even with rename: source contents, mtime, ID and history remain
unchanged. Explicit target ownership/accessACL is supported.

Cross-root move requires two managed roots and an application-generated canonical
UUID `id`. Persist it before requesting the move. The destination owns status,
resume and abandon. Pending HTTP 202 is not completed success:

- `prepared`: recorded, no confirmed destination publication.
- `source_pending`: target published, source removal not durably confirmed; after
  a crash the source may already be absent.
- `completed`: both phases recorded.
- `abandoned`: no further deletion intent; current files untouched, removed
  sources are not restored.

Same UUID + changed request conflicts. Explicit resume restores the stored Unix
identity and checks root-wide content/namespace generations. Unrelated changes
can cause 412 and prevent source deletion. Never automatically delete during
startup or assume a whole cross-root transaction is atomic. At most 128 unresolved
receipts per destination; pending never expires. Terminal records last at least
seven days, possibly longer for outstanding acknowledgement cleanup. Use abandon
to resolve an unsafe pending intent; inspect both files first.
Changing either root's managed setting invalidates pending source-deletion
checks; ordinary restart with unchanged settings preserves them. If source
deletion is already durably confirmed, resume can finalize only the receipt even
after managed/execution capabilities are disabled; it performs no file operation.

Versions snapshot old contents before overwrite. The default one-minute cooldown starts
at the last successful snapshot; skipped writes do not reset it. Manual snapshots
and restore bypass cooldown; there is no noVersion flag. Upload metadata follows
the incoming revision. Metadata is a JSON object <=8192 serialized UTF-8 bytes.
Pinned versions survive automatic pruning, not explicit or permanent file deletion.

Retention unions last/hourly/daily/weekly/monthly counts using UTC calendar
buckets; weekly starts Monday. Pins do not consume tier budgets. Missing/zero
fields keep nothing for that tier. Absent keep section defaults to
`{last:10,daily:30,monthly:12}`; `keep:{}` retains only pins. Pruning selects
existing versions; it does not schedule snapshots.

## Browsing and statistics

ListingOptions: sort=`path|name|size|modified` (default path), order=`asc|desc`
(default asc), type=`all|files|directories` (default all), limit 1–1000 (default 100),
maxEntries 1–100000 (default 100000), opaque after. Stable path ties follow the same
order. Filter/sort precedes pagination. List returns immediate children; search walks
descendants, excluding its base.

Indexed scans seek forward; an empty page can still have next. Filesystem scans
sort one bounded observation: 60-second lifetime, 16 snapshots / 32 MiB per root,
100000 entries max. Exceeded budget returns 413, not a globally sorted partial
result. Scoped Unix listings use filesystem observations even on indexed roots.
Keep query parameters unchanged. Mutations/rebuild/restart/query change/
expiry/eviction can return 409 cursor_invalid: discard pages and restart.

RecursiveStats performs one subtree walk; includes the selected directory count.
Inspect `complete`, `freshness`, `source`, `started`, `completed`, `path` and optional
`indexBuilt`. Partial budget results set complete=false. Observed freshness is a
scan interval, not atomic quota; cached index totals are unknown. Null is not zero.
Subtree stats do not replace the root cache. Root RefreshStats fails 413 if incomplete.

## Unix identities, ownership and ACLs

Backend header `X-Filegate-Execution` is one JSON object `{uid, gid, groups?}`, at most 4096
bytes. UID 1–4294967294, GID/groups 0–4294967294, <=64 supplementary groups sorted
and deduplicated. Unknown fields invalid. Identity selects execution rights;
Ownership selects resulting owner metadata. Resolve identities in the application.

Kernel checks apply to traversal, content opens and public mutations under that
identity. No privileged fallback on denial. Same identity covers all copy/move/ZIP
roots. Each root must enable execution. Leases sign it; sessions store it immutably.
Browser execution headers return 400 execution_override_not_allowed. Session scope
mismatch 403 execution_mismatch; disabled root 409; denied filesystem 403.

Metadata reads require traversal but need not grant content access. Live
chmod/chown/ACL changes run under the actor. New staged uploads/directories/parents
may receive explicit trusted Ownership/accessACL under service privileges before
actor-authorized publication. Default owner is actor UID/GID when scoped, otherwise
daemon; setgid/default ACL inheritance applies. Overwrites preserve owner/accessACL
unless explicitly replaced, and clear regular-file setuid/setgid bits. Explicit
mode is applied last and can change the ACL mask.

File `mode` is an octal string <=0777; directory `dirMode` also permits setgid,
for example `"2770"`. UID/GID must appear together. Access/default ACLs work without index.
Each nonempty ACL requires owner, owningGroup, other; named user/group IDs require
mask. Permissions are three-character rwx patterns. PUT accepts 3–256 entries; empty PUT
invalid. GET default may return `{entries: []}` when absent; unsupported returns 501.
`clearDefaultACL` removes future inheritance only. No automatic recursive fixes.

Native replacement checks parent write/search and sticky rules, not an old
leaf's write bit. Moving a directory between parents also requires write permission
on that directory. Recursive removal authorizes detaching the selected entry;
private cleanup is not a recursive rm permission walk. Historical content checks
current-file read access; historical ACLs are not stored. Open descriptors are
not retroactively revoked by chmod. NFS credentials/export/root_squash/Kerberos
rules still apply; local UID switching cannot override them.

Search/dashboards/index/stats/prune reject execution scope; use the unscoped
backend. Results are not filtered by Unix user rights. Worker capacity returns 503
execution_capacity with Retry-After: 1.

## Downloads and previews

Leases default 60 seconds, maximum 300; expiry controls admission, not completion.
They are reusable and have no individual revoke list or callbacks. Token rotation
plus restart invalidates all. Historical leases bind path/file identity/version;
thumbnail leases bind path/dimensions. Query parameters cannot broaden scope.

Current/historical downloads use attachment headers with ASCII filename fallback
and RFC 5987 UTF-8 filename*. DownloadOptions.fileName is UTF-8 at most 255 bytes, no controls
or slash/backslash, not dot/dotdot. Defaults sanitize the path basename. Range and
HEAD work for current/historical bytes; JPEG previews ignore Range. CORS exposes
Content-Length, Content-Range, Content-Disposition, ETag, Retry-After.

Thumbnails accept JPEG/PNG/GIF, at most 64 MiB input, 40 million pixels, bounds 1–2048
(default 256), no upscaling, JPEG quality 85, at most 16 MiB output. Four renders and 32
in-flight duplicate waiters are bounded; every caller source-open is authorized.
No persistent cache. Capacity 503 thumbnail_capacity includes Retry-After: 1.
Cancellation is cooperative between rendering stages.

`files.archiveLease(items,expiresIn?)` / `Client.ArchiveLease(ctx,items,expiresIn)`
bind the exact selection manifest. Browser `downloadArchive(lease)` posts a native
form; archiveRaw streams unchanged HTTP responses. Fetch-based direct helpers use
`credentials: "omit"`; native forms cannot suppress target-origin cookies. Use a
dedicated Filegate origin outside the scope of application cookies. Direct POST body has exactly
one `manifest` field. Directory selection includes its current subtree; ZIP is
not a snapshot. Actor denial/read failure aborts rather than silently skips files.

Limits: 1000 selections, 128 KiB manifest, 10000 entries, 20000 scanned objects, depth 64,
100 GiB bytes, four streams. Names are safe portable UTF-8 paths; normalized/casefold
exact or nested archive-name collisions rejected. Same source under distinct
archive names is allowed. Symlinks/private paths rejected; implicit private entries
excluded. ZIP is uncompressed, no HEAD/Range.

## Operations

Static strict YAML, applied on restart. Example:

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
    managed: true
    index: true
    versioning:
      enabled: true
      cooldown: 1m
      keep: {last: 10, daily: 30, monthly: 12}
  - name: shared
    path: /mnt/shared
    index: false
```

Roots must exist; state must be outside all roots. Root name/path and writable
state binding are durable. One daemon per writable root set. Do not repoint a name,
run competing daemons, or delete state as index repair. Token: 32–4096 bytes, operator
provided, stored privately. TLS at proxy, exact allowed browser origins, no signed
URL logging. NFS requires actual mount/export/locking verification.

Packages for Debian/Rocky include systemd and example config. Default account
filegate:filegate is unprivileged. To enable execution on a new deployment,
explicitly opt into root service with a systemd drop-in `User=root`, `Group=root`;
keep NoNewPrivileges=true and sandboxing. Root paths must be in ReadWritePaths.
`fs.suid_dumpable` must be 0 or 2, never 1. Container default non-root does not enable it.
Do not change production identity/capabilities/permissions without authorization.

Before changing an existing service identity, stop and back up. Private `.filegate`
must be owned by the new daemon UID, mode 0700; state stays service-accessible.
Adjust private data only; do not recursively chown user data. CAP_CHOWN alone
cannot provide arbitrary reads or credential switching.

CLI `validate`, `status`, `roots`, `rebuild ROOT`, `stats ROOT`, `prune ROOT`.
Administrative commands call the daemon using public_url/token; only daemon opens
live state. Rebuild pauses root mutations; other roots remain available. External
writers need separate coordination. Maintenance runs every five minutes.

Reserve about twice a session's size for segments plus assembly, plus history,
existing files and concurrent uploads. Application owns aggregate budgets.

Back up stopped/coordinated roots including `.filegate`, state, config and token.
Preserve inode identity, ownership, ACLs and xattrs for full history rollback.
Ordinary copied files on new inodes do not reconnect old histories; import current
contents into a new root/state and retain the original backup. Protect root-level
`.filegate` and `LOCK` from external replacement. Startup recovers local intents;
cross-root source deletion requires explicit resume.

## Reference

Use current SDK types and the documented contract together. For detailed routes,
examples and operator procedures:

- [HTTP API](https://filegate.dev/docs/en/http-api)
- [TypeScript](https://filegate.dev/docs/en/ts-sdk) and [Go](https://filegate.dev/docs/en/go-sdk)
- [Sessions and downloads](https://filegate.dev/docs/en/uploads-downloads)
- [Copy/move recovery](https://filegate.dev/docs/en/transfers)
- [Browsing and totals](https://filegate.dev/docs/en/browsing)
- [Permissions](https://filegate.dev/docs/en/permissions)
- [Versions](https://filegate.dev/docs/en/versioning)
- [Configuration](https://filegate.dev/docs/en/configuration) and [operations](https://filegate.dev/docs/en/operations)
