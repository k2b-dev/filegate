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
const upload = await files.root("cloud").directUpload("homes/alex/note.txt", 5, {
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

An overwrite preserves existing ownership when no ownership is supplied. New
files use the daemon account with mode 0644; newly created parents request 0755, reduced by the daemon umask (0750 with the
packaged systemd unit).
Explicit UID and GID must be supplied together. Modes are octal strings limited
to permission bits. `dirMode` applies to newly created directories. Existing
parent directory permissions are not changed.

## Large files: resumable sessions

```ts
// Backend:
const session = await files.root("cloud").createSession("videos/demo.mp4", size, {
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
