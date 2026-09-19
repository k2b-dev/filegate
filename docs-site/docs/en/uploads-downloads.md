---
title: Direct transfers
section: Use
order: 20
description: Short-lived transfer leases, backend-controlled upload sessions and ZIP selections.
---

# Direct transfers

Your backend authorizes a user and issues a scoped transfer lease. The browser
sends bytes directly to Filegate. The daemon bearer token stays on the backend.
Leases expire after 60 seconds by default; the maximum is 300 seconds.

A lease authorizes requests started before its expiry. An accepted download can
finish afterward. Leases are reusable until expiry, cannot be individually
revoked and do not call your application to recheck access. Keep signed URLs out
of logs and analytics. The application owns users, public links and storage budgets.

Roots with `execution: true` can bind numeric Unix credentials to transfers.
Create leases or sessions from a scoped backend client (`root.as(identity)` in
TypeScript, `root.WithExecution(identity)` in Go). Browsers use the returned lease
without sending an identity header. Renewal and commit retain the session's
original identity. See [Unix permissions](/docs/en/permissions#unix-execution-identity).

## Upload and publish separately

Use sessions when your backend must approve publication, including for small or
empty files. A session lasts 24 hours. Its short-lived lease permits status
queries and segment writes; abort is optional. Only the authenticated backend can
commit the file or issue another lease. Renewing a lease does not extend the session.

```ts
// Backend, after authorizing the destination and reserving the upload budget:
const created = await files.root("documents").createSession("videos/demo.mp4", size, {
  onConflict: "error",
});
// Return created.session.id and created.lease to the authorized browser.

// Browser:
import { DirectSession } from "@k2b/filegate/utils";
if (!created.lease) throw new Error(`Session is already ${created.session.state}; reconcile its result`);
const transfer = new DirectSession(created.lease.url);
await transfer.upload(file, { onProgress: (done, total) => console.log(done, total) });
// Ask your application backend to commit. Uploading alone does not publish.
```

Before committing, the backend checks access and budget again. For public inboxes,
choose unique destination paths on the backend and use `onConflict: "error"`.
Reserve the declared size before upload, then reconcile against the recorded
commit result. Direct PUT uploads publish immediately and do not provide this
approval step.

Segments are 8 MiB except the final one, with at most 10,000 segments. Empty files
need no segments. Repeating the same segment bytes is safe; different bytes at an
already uploaded segment return 409. Status reports `received` bytes and
`uploadedSegments`; acknowledged hashes are retrieved through paged segment
receipts, independently of the status response.
Commit verifies lengths and hashes, then publishes the complete file. A segment
accepted before lease expiry can finish only while its session remains open.

| Session state | Meaning |
| --- | --- |
| `open` | Segments can be uploaded; the backend can commit or abort. |
| `committed` | The file was published; `result` contains the original Node. |
| `aborted` | The session was cancelled and cannot publish. |
| `expired` | The 24-hour session deadline passed without publication. |

Terminal records remain queryable for seven days after completion, or after the
session deadline for expired sessions. Commit retries return the original result,
including its actual size, final path and optional indexed ID, even if the file
has since moved or been deleted. This remains true after daemon restart or index
rebuild. Aborting a committed session returns `409 session_committed`; it does not
remove the published file or receipt. Repeated aborts are safe. Concurrent commit
and abort requests produce one terminal outcome.

After a lost response, query the backend session endpoint or retry commit.
After receipt retention ends, 404 means the outcome is no longer known; it does
not prove that publication failed. Browser status exposes transfer progress and
state, not backend write options or commit metadata.

When a lease expires, ask your backend for another one after reauthorization,
then resume the same session. Do not create a new session merely to renew access.

## Recover session creation and segments

Set an `idempotencyKey` when creating a session if the first response may be lost.
Retry with the same key, path, size, write options and execution identity to get
the same session while its record remains available. Reusing a key for a different
request returns 409. Keys are optional strings of at most 128 bytes, without
leading or trailing whitespace; generate a fresh UUID for each logical upload.

Creation returns `{session, lease?}`. An open session receives a new short-lived
lease. A recovered terminal session has no lease: inspect its state and recorded
result instead of starting another upload. Session deadlines and receipt retention
are unchanged by a retry.

Use `root.sessionSegments(id, after, limit)` on the backend or
`DirectSession.segments(after, limit, signal)` in the browser. Pages contain
`{items: [{index, hash}], next?}` in ascending segment order. Start with `after: -1`,
then pass `next` unchanged until absent. The default limit is 100, maximum 1000.
Terminal sessions return an empty segment page; use their terminal result.

`DirectSession.upload` checks each acknowledged hash against the supplied file
once, then sends missing segments. An `AbortSignal` cancels local requests and
retry waits; it does not abort the stored session. `abort()` explicitly changes
the session state and requires an abort-enabled lease. Browser helpers retry GET
and replayable segment PUT requests at most twice for transport failures or
429/502/503/504. Retry waits are bounded to two seconds; a longer `Retry-After`
is returned to the caller without an earlier retry. Non-replayable bodies,
explicit aborts and lease expiry are not retried automatically.

## Publish with one PUT

Use a direct PUT when the backend can authorize publication before uploading.

```ts
const upload = await files.root("documents").directUpload("homes/alex/note.txt", 5, {
  onConflict: "overwrite",
  ownership: { uid: 10042, gid: 10042, mode: "0640", dirMode: "0750" },
  metadata: { message: "Updated note" },
});
// Browser:
const response = await fetch(upload.url, { method: "PUT", body: "hello", credentials: "omit" });
if (!response.ok) throw new Error(`Upload failed: ${response.status}`);
```

The lease binds the root, path, exact byte count, conflict policy, ownership,
access ACL, metadata, publication condition, execution identity and expiry. The uploader cannot override those fields. Reusing a URL
before expiry repeats the authorized operation: single PUT URLs are not one-shot
or resumable. Default conflict behavior is `error` (409); `overwrite` and
`rename` must be explicit. Rename uses a random suffix and returns the actual
path; it does not search for the next numbered filename. See
[copy and move conflicts](/docs/en/transfers#choose-a-conflict-policy).

## Publish only if unchanged

On a root configured with `managed: true`, read the file's opaque `revision`
before editing, then bind it to the upload:

```ts
const current = await root.stat("notes.txt");
if (!current.revision) throw new Error("A managed regular file is required");
const upload = await root.directUpload("notes.txt", updated.size, {
  onConflict: "overwrite",
  precondition: { ifMatch: current.revision },
});
```

Use `precondition: { ifNoneMatch: true }` for create-only publication. Choose
exactly one condition. Conditions cannot use `onConflict: "rename"`, and
`ifNoneMatch` cannot use `overwrite`. Both direct PUT and session commit check
the condition atomically with publication against other Filegate operations.

A mismatch returns `412 precondition_failed` without changing the destination,
creating parents, assigning IDs or capturing history. A session remains open
with its acknowledged segments, so the backend can inspect the conflict. The
condition itself is immutable; choosing a different condition requires a new
lease or session.

The condition is signed into a direct lease or stored in a session. A browser
cannot add or weaken it. Optional `If-Match` or `If-None-Match` headers on the
publishing request must exactly match the bound condition: a quoted revision or
`*`, respectively. An unbound or different header returns
`400 precondition_header_mismatch`. Omitting headers still enforces the bound
condition. Stat, current-content and successful publication responses expose a
quoted `ETag` on managed regular files.

Managed roots require all writers to use Filegate. This setting is independent
of indexing. A live filesystem fingerprint detects observed external inode,
size, nanosecond modification-time and change-time differences, but cannot make
an external NFS writer participate in an atomic check-and-replace. Keep managed
mode off for roots with external writers; conditional publication there returns
`409 feature_disabled`. Metadata changes can also invalidate a revision.

## Ownership and inherited permissions

Direct uploads and session commits use the same ownership and ACL rules. An
overwrite preserves the existing owner, group, ordinary permission bits and
access ACL unless the request explicitly changes them. Replacing file contents
clears setuid and setgid bits. New files inherit the destination directory's
default ACL and, when setgid is enabled, its group. Explicit UID/GID overrides
group inheritance. Without a default ACL, new files default to 0644 and new directories to 0755.
With a default ACL, the creation limits are 0666 for files and 0777 for directories;
ordinary file uploads do not inherit execute permission. An explicit mode is
applied afterward and can widen or restrict the ACL's effective permissions.

Explicit UID and GID must be supplied together. Modes are octal strings:
`mode` accepts file permissions up to `0777`; `dirMode` also accepts setgid, such
as `"2770"`. Existing parent directories are not modified. Explicit mode changes
also affect the access ACL's effective permissions. See
[permissions and ACLs](/docs/en/permissions) for shared-directory setup.

## Transfer capacity

Uploads are bounded by `uploads.max_file_size`. Up to 16 direct PUT requests,
including segment writes, run concurrently; excess requests return 503 and may
be retried. Partial bodies and failed commits do not publish partial files.

Session assembly temporarily needs space for both the uploaded segments and the
complete file: roughly twice the upload size. Allow additional space for existing
files, versions and concurrent uploads. Filegate enforces individual upload sizes;
your application manages aggregate budgets. Maintenance cleans expired sessions
and obsolete receipts every five minutes.

## Downloads and previews

`directDownload(path)` issues a GET lease with HEAD and HTTP Range support. It
authorizes the contents found at that path when used; it is not an immutable
revision link.

For historical contents, use `directVersionDownload(path, versionId, options?)`.
It binds that version to its root and file path. A deleted version or a file that
has moved is no longer available through the URL.

For previews, use `directThumbnail(path, { width, height, expiresIn })`. Dimensions
default to 256 × 256 and are fixed by the lease. The preview uses the contents at
the path when requested. It fits within the requested bounds without upscaling.

```ts
// Backend, after authorizing each resource:
const version = await root.directVersionDownload("report.pdf", versionId);
const preview = await root.directThumbnail("photo.png", { width: 320, height: 180 });
// Return the leases to the browser; keep the backend token private.

// Browser:
const response = await fetch(version.url, { credentials: "omit" });
if (!response.ok) throw new Error(`Version download failed: ${response.status}`);
const bytes = await response.blob();
const image = document.createElement("img");
image.src = preview.url;
document.body.append(image);
```

Both URLs support HEAD. Versions support HTTP Range; thumbnails ignore Range and
return the whole JPEG. Current and historical downloads set attachment filenames;
thumbnails do not set Content-Disposition. Appending path,
version or image parameters to the URL cannot change its scope. The usual
60-second default and 300-second maximum apply; a lease does not retain a version
against deletion or pruning. See the [HTTP contract](/docs/en/http-api#version-and-thumbnail-download-leases)
for response headers and errors.

Backend clients can stream `contentRaw`, `versionContentRaw` and `thumbnailRaw`.
Raw methods return HTTP responses unchanged, including error statuses; check the
status and close/drain response bodies in Go. Thumbnails accept JPEG, PNG and GIF,
up to 64 MiB and 40 million decoded pixels; requested bounds are at most 2048 × 2048.

Current and historical download options accept `fileName`, for example
`root.directVersionDownload("report.pdf", versionId, { fileName: "Approved report.pdf" })`.
The lease binds this name. Responses include a quoted ASCII `filename` fallback
and UTF-8 `filename*`. Names must be valid UTF-8, at most 255 bytes, and contain
no control characters or path separators; `.` and `..` are invalid. Without an
override, Filegate uses a sanitized basename. A browser cannot change the signed
name with a query parameter.

Thumbnail capacity failures return `503 thumbnail_capacity` with `Retry-After: 1`.
At most four renders run concurrently, with 32 callers waiting for identical
in-flight results. Every caller opens its source under its own access rules
before sharing a result. Results are not persistently cached. Input is bounded
to 64 MiB and 40 million pixels, and each generated JPEG to 16 MiB. Cancellation
stops waiting and cancels unnecessary work between render stages; a running image
transform can finish before its capacity slot is released.

## Use an internal transfer origin

A backend can configure a trusted internal origin for its own direct transfers,
while browsers continue to receive public lease URLs:

```ts
const internal = new Filegate({
  baseUrl: "https://files.example.org",
  token,
  transferBaseUrl: "http://filegate.internal:8080",
});
const lease = await internal.root("documents").directDownload("report.pdf");
const response = await internal.downloadRaw(lease);
```

`put`, `downloadRaw`, `archiveRaw` and `directSession` use the configured transfer
origin. Lease issuance and returned public URLs are unchanged. Direct requests
send neither the backend token nor an execution header. The setting is operator
configuration for Filegate-issued leases, not an arbitrary URL-import feature.

## Download a ZIP selection

The backend submits an authorized selection to `POST /v1/downloads/archives`:

```json
{
  "items": [
    { "root": "documents", "path": "reports/annual.pdf", "archivePath": "annual.pdf" },
    { "root": "shared", "path": "photos", "archivePath": "photos" }
  ]
}
```

Filegate returns a short-lived POST URL and an exact `manifest` string. Submit
that string unchanged as the `manifest` field in an
`application/x-www-form-urlencoded` POST. A native browser form can download the
response directly without buffering the archive in JavaScript. The lease binds
the manifest hash; changing any selection or archive name invalidates the request.

Fetch-based direct helpers set `credentials: "omit"`. Native form downloads
cannot suppress browser cookies for the target origin. Serve Filegate on a
dedicated origin outside the scope of application cookies when using
`downloadArchive` or other native browser requests.

A selected directory includes its entire current subtree. Authorize the whole
subtree before issuing the lease. With a bound Unix execution identity, every
source open also checks that identity's permissions; any denied entry fails the
archive. ZIP selections can span roots. Archive names must be
safe relative paths, with no duplicates, nested selection names or case-insensitive
collisions. Symbolic links and explicit private Filegate selections are rejected.
Private Filegate entries are excluded when traversing a directory.

Limits are 1,000 selections, a 128 KiB manifest, 10,000 expanded entries, 20,000
scanned objects (including excluded private entries), depth 64, 100 GiB of file
contents and four concurrent archive streams. Archive names must be valid UTF-8
and safe on Windows; Unicode-normalized names are compared without case. Use
`path: "."` to select a root; an empty path is invalid. ZIP files use no
compression and do not support Range requests. A selection is not a snapshot;
concurrent filesystem changes can change the downloaded contents or fail the
transfer. A read error aborts the stream instead of silently omitting an entry.
