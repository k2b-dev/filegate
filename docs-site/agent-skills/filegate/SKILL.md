---
name: filegate
description: Integrate or operate Filegate, the Linux filesystem gateway. Use for the @k2b/filegate TypeScript client, Go SDK, root-scoped HTTP API, direct uploads and resumable sessions, version history, Unix ownership, systemd deployment, static configuration, index rebuilds, dashboard metadata, backups or recovery. The application owns user authorization; Filegate serves independent named roots.
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
- Numeric ownership comes from the trusted backend. For new files, omitted
  ownership uses the daemon account. Arbitrary chown requires OS privileges.

## Integration rules

1. Keep the full bearer token on trusted backends. Authorize the application user
   before issuing a scoped direct upload/download/session URL.
2. Prefer small direct PUTs and resumable sessions for large files. Both bind
   ownership, metadata and conflict policy when the backend creates the transfer.
3. Default conflict policy is `error`; choose `overwrite` or `rename` explicitly.
4. Use relative paths, never absolute server paths. Store `{root,id}` for indexed
   identity references, or `{root,path}` when indexing is off.
5. Raw stream methods preserve HTTP error responses. Check status and close Go
   response bodies. Never buffer a large relay into memory.
6. Do not delete `state_dir` to rebuild. Only the daemon owns live writable state;
   CLI rebuild/prune/stats calls its API.
7. Use the HTTP contract below and current API types to select routes and request
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
instead, suitable for an authorized browser. `createSession` returns a scoped
session URL for larger uploads. Import token-free browser helpers from
`@k2b/filegate/utils`: `DirectSession`, `putDirect`, `segments` and `sha256`.

### Root operations

| Method | Result or action |
| --- | --- |
| `info()` | Root configuration, capabilities and dashboard metadata. |
| `stat(path)` | Current filesystem metadata; optional indexed identity. |
| `resolve(id)` | Current path for an indexed file ID. |
| `list(path, { after, limit })` | Alphabetical page of immediate children. |
| `search(q, { path, after, limit, maxEntries, signal })` | Case-insensitive filename substring search. |
| `mkdir(path, ownership?)` | Create a directory and missing parents. |
| `setOwnership(path, ownership)` | Apply Unix ownership/mode without creating a version. |
| `remove(path, recursive?)` | Permanent deletion including histories. |
| `transfer(path, targetRoot, targetPath, options)` | Copy; set `move: true` for a move. |
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

Streaming methods `contentRaw`, `archiveRaw`, `thumbnailRaw` and
`versionContentRaw` do not throw on HTTP error responses. Typed JSON methods
throw `FilegateError` with `status`, `code` and `message`. Supply an optional
`fetch` in the constructor for testing or transport customization.

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
`DirectUpload(ctx, path, size, options)` and return its URL. The byte count must
match exactly. `filegate.PutDirect(ctx, url, reader, size)` sends no bearer token.

### Resumable upload

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

### API map

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
| `GET /archive` | `path` | TAR stream. |
| `GET /thumbnail` | `path`, `width`, `height` | JPEG preview. |
| `POST /directories` | `{path,ownership?}` | Created Node. |
| `PATCH /ownership` | `path`; body `{uid?,gid?,mode?,dirMode?}` | Updated Node. |
| `DELETE /files` | `path`, `recursive` | 204. |
| `POST /transfers` | `{path,targetRoot,targetPath,move?,onConflict?,ownership?,metadata?}` | Destination Node. |
| `POST /uploads/direct` | `{path,size,expiresIn?,onConflict?,ownership?,metadata?}` | `{url,method,expires}`. |
| `POST /downloads/direct` | `{path,expiresIn?}` | `{url,method,expires}`. |
| `POST /uploads/sessions` | `{path,size,onConflict?,ownership?,metadata?}` | Session plus scoped `url`. |
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
`mode`, `uid` and `gid`. Timestamps are RFC 3339, sizes are integer bytes. Directory
size is zero; recursive totals belong to stats. `limit` defaults to 100 and is
bounded by 1000. Search/stats traversal defaults to 100,000 entries and accepts
an explicit maximum of 10,000,000.

### Scoped session URL

Use the exact URL returned at session creation:

| Method | Meaning |
| --- | --- |
| `GET URL` | Status and received segment hashes. |
| `PUT URL?segment=N` | Exact segment bytes. |
| `POST URL` | Commit; repeated commits return the recorded result. |
| `DELETE URL` | Abort/retire the session. |

Raw stream routes return normal HTTP statuses. Common JSON statuses are 400 for
invalid input, 401 for authentication/capability failure, 403 for permissions,
404 for missing files, 409 for conflicts/disabled features, 413 for limits and
503 for concurrent transfer capacity. Do not retry a mutation blindly after an
ambiguous transport failure: session commits are idempotent, while ordinary
mutations require reading back the resulting state.

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
sessions every five minutes. There is no automatic scan for external file changes.

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
Assess required privileges and NFS root-squash on the actual deployment. Do not
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
and snapshot all roots and state together, preserving xattrs and inode identity.
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
