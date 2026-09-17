---
title: Direct transfers
section: Use
order: 20
description: Small direct uploads, resumable sessions, download URLs and ownership.
---

# Direct transfers

Your backend authorizes a user and mints a scoped URL. The browser sends bytes
directly to Filegate. The daemon bearer token stays on the backend.

## Small files: one PUT

```ts
const upload = await files.root("documents").directUpload("homes/alex/note.txt", 5, {
  onConflict: "overwrite",
  ownership: { uid: 10042, gid: 10042, mode: "0640", dirMode: "0750" },
  metadata: { message: "Updated note" },
});
// Browser:
await fetch(upload.url, { method: "PUT", body: "hello" });
```

The URL binds the root, path, exact byte count, conflict policy, ownership,
metadata and expiry. The uploader cannot override those fields. It expires after
15 minutes by default; the backend can request up to 24 hours. Reusing a URL
before expiry repeats the authorized operation: single PUT URLs are not one-shot
or resumable. Default conflict behavior is `error` (409); `overwrite` and
`rename` must be explicit. Rename appends `-01`, `-02`, … before the extension.

Direct uploads and session commits use the same ownership and ACL rules. An
overwrite preserves the existing owner, group, ordinary permission bits and
access ACL unless the request explicitly changes them. Replacing file contents
clears setuid and setgid bits. New files inherit the destination directory's
default ACL and, when setgid is enabled, its group. Explicit UID/GID overrides
group inheritance. Without a default ACL, files default to 0644 and new parents
request 0755, reduced by the daemon umask (0750 with the packaged systemd unit).
With a default ACL, the creation limits are 0666 for files and 0777 for directories;
ordinary file uploads do not inherit execute permission. An explicit mode is
applied afterward and can widen or restrict the ACL's effective permissions.

Explicit UID and GID must be supplied together. Modes are octal strings:
`mode` accepts file permissions up to `0777`; `dirMode` also accepts setgid, such
as `"2770"`. Existing parent directories are not modified. Explicit mode changes
also affect the access ACL's effective permissions. See
[permissions and ACLs](/docs/en/permissions) for shared-directory setup.

## Large files: resumable sessions

```ts
// Backend:
const session = await files.root("documents").createSession("videos/demo.mp4", size, {
  onConflict: "overwrite",
});
// Browser, given only session.url:
import { DirectSession } from "@k2b/filegate/utils";
const transfer = new DirectSession(session.url);
await transfer.upload(file, { onProgress: (done, total) => console.log(done, total) });
```

Sessions expire after 24 hours. Segments are 8 MiB except the final one, with at
most 10,000 segments. Repeating the same segment bytes is safe; different bytes
at an already uploaded segment return 409. Session status reports received
segments. Commit verifies their lengths and hashes, then publishes atomically.
Commit retries return the original result until session expiry, including after
a daemon restart or index rebuild. Abort removes staging data; aborting a
completed session does not delete the published file.

Uploads are bounded by `uploads.max_file_size`. Up to 16 direct PUT requests
(including segment writes) run concurrently; excess requests return 503 and may
be retried. Partial request bodies and failed commits do not publish partial files.

## Download, previews and archives

`directDownload(path)` mints a GET URL for the path, with HEAD and HTTP Range
support. It authorizes the contents found at that path when used; it is not an
immutable revision link. Keep URLs out of logs and analytics.

Backend clients can stream `contentRaw`, `versionContentRaw`, `thumbnailRaw`
and `archiveRaw`. Raw methods return HTTP responses unchanged, including error
statuses; callers must check the status and close/drain response bodies in Go.
Thumbnails accept JPEG, PNG and GIF, up to 64 MiB and 40 million decoded pixels;
requested bounds are at most 2048 × 2048. Archives stream TAR files.
