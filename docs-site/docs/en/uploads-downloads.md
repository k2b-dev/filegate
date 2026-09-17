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
already uploaded segment return 409. Status reports received segment hashes.
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

## Publish with one PUT

Use a direct PUT when the backend can authorize publication before uploading.

```ts
const upload = await files.root("documents").directUpload("homes/alex/note.txt", 5, {
  onConflict: "overwrite",
  ownership: { uid: 10042, gid: 10042, mode: "0640", dirMode: "0750" },
  metadata: { message: "Updated note" },
});
// Browser:
const response = await fetch(upload.url, { method: "PUT", body: "hello" });
if (!response.ok) throw new Error(`Upload failed: ${response.status}`);
```

The lease binds the root, path, exact byte count, conflict policy, ownership,
metadata and expiry. The uploader cannot override those fields. Reusing a URL
before expiry repeats the authorized operation: single PUT URLs are not one-shot
or resumable. Default conflict behavior is `error` (409); `overwrite` and
`rename` must be explicit. Rename appends `-01`, `-02`, … before the extension.

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

For historical contents, use `directVersionDownload(path, versionId, expiresIn?)`.
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
const response = await fetch(version.url);
if (!response.ok) throw new Error(`Version download failed: ${response.status}`);
const bytes = await response.blob();
const image = document.createElement("img");
image.src = preview.url;
document.body.append(image);
```

Both URLs support HEAD. Versions support HTTP Range; thumbnails ignore Range and
return the whole JPEG. Neither response sets Content-Disposition. Appending path,
version or image parameters to the URL cannot change its scope. The usual
60-second default and 300-second maximum apply; a lease does not retain a version
against deletion or pruning. See the [HTTP contract](/docs/en/http-api#version-and-thumbnail-download-leases)
for response headers and errors.

Backend clients can stream `contentRaw`, `versionContentRaw` and `thumbnailRaw`.
Raw methods return HTTP responses unchanged, including error statuses; check the
status and close/drain response bodies in Go. Thumbnails accept JPEG, PNG and GIF,
up to 64 MiB and 40 million decoded pixels; requested bounds are at most 2048 × 2048.

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

A selected directory includes its entire current subtree. Authorize the whole
subtree before issuing the lease; selecting a folder cannot enforce permissions
on individual descendants. ZIP selections can span roots. Archive names must be
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
