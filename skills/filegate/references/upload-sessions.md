# Upload Sessions / Resumable Uploads

Use upload sessions for files that need resume, parallel segment delivery,
progress reporting, or direct browser upload without a Filegate bearer token.

## State machine

```
client                                      server
──────                                      ──────
POST /v1/uploads/sessions
{ path, size, checksum, segmentSize?,
  onConflict?, direct? }
                  ───────────────────────► [persist session in Pebble]
                                           [return segment plan]
                  ◄─────────────────────── { id, segments, uploadedSegments, direct? }

PUT /v1/uploads/sessions/{id}/segments/0
X-Segment-Checksum: sha256:...
<segment bytes>
                  ───────────────────────► [store segment idempotently]
                  ◄─────────────────────── { uploadedSegments: [...] }

POST /v1/uploads/sessions/{id}/commit
                  ───────────────────────► [assemble, verify whole checksum]
                                           [atomic replace/create]
                  ◄─────────────────────── { node, checksum }
```

Segments can arrive in any order. Duplicate segment PUTs are accepted when
the bytes match the original segment. Commit is explicit and safe to retry
after it has succeeded.

## Resume from interruption

```ts
const status = await fg.uploads.sessions.status({ sessionId });
const done = new Set(status.uploadedSegments);

for (const segment of status.segments) {
  if (done.has(segment.index)) continue;
  const slice = file.slice(segment.offset, segment.offset + segment.size);
  const bytes = new Uint8Array(await slice.arrayBuffer());
  await fg.uploads.sessions.segments.put({
    sessionId,
    index: segment.index,
    body: slice,
    checksum: await uploads.checksum.sha256(bytes),
  });
}

await fg.uploads.sessions.commit({ sessionId });
```

## TypeScript flow

```ts
import { Filegate } from "@k2b/filegate/client";
import { uploads } from "@k2b/filegate/utils";

const fg = new Filegate({ baseUrl, token });
const file: File = pickedFile;
const segmentSize = 8 * 1024 * 1024;

// Use an incremental SHA-256 implementation for multi-GB files. The helper
// below is fine for small examples because it hashes one Uint8Array.
const whole = new Uint8Array(await file.arrayBuffer());
const session = await fg.uploads.sessions.create({
  path: `data/uploads/${file.name}`,
  size: file.size,
  checksum: await uploads.checksum.sha256(whole),
  segmentSize,
  onConflict: "error",
});

const concurrency = 4;
let next = 0;
async function putOne(segment: (typeof session.segments)[number]) {
  const blob = file.slice(segment.offset, segment.offset + segment.size);
  const bytes = new Uint8Array(await blob.arrayBuffer());
  await fg.uploads.sessions.segments.put({
    sessionId: session.id,
    index: segment.index,
    body: blob,
    checksum: await uploads.checksum.sha256(bytes),
  });
}

await Promise.all(Array.from({ length: concurrency }, async () => {
  for (;;) {
    const segment = session.segments[next++];
    if (!segment) return;
    await putOne(segment);
  }
}));

const committed = await fg.uploads.sessions.commit({ sessionId: session.id });
console.log(committed.node.id);
```

## Direct browser upload session

The backend creates the session with `direct: {}` and returns only the direct
token object to the browser. The browser can upload and commit without seeing
the Filegate bearer token.

```ts
// backend
const session = await fg.uploads.sessions.create({
  path: "data/inbox/photo.jpg",
  size: fileSize,
  checksum,
  segmentSize: 8 * 1024 * 1024,
  direct: { allow: ["putSegment", "status", "commit", "abort"] },
});
return Response.json({ session });

// browser
import { directUploads } from "@k2b/filegate/client";

await directUploads.segments.put({
  direct: session.direct!,
  index: 0,
  body: firstSegment,
  checksum: firstSegmentChecksum,
});
await directUploads.commit({ direct: session.direct! });
```

## Go flow

```go
import (
    "bytes"
    "io"
    "os"

    "github.com/k2b-dev/filegate/v3/sdk/filegate"
    "github.com/k2b-dev/filegate/v3/sdk/filegate/segments"
)

f, err := os.Open(srcPath)
if err != nil { return err }
defer f.Close()

info, err := f.Stat()
if err != nil { return err }

checksum, err := segments.SHA256Reader(f)
if err != nil { return err }
if _, err := f.Seek(0, io.SeekStart); err != nil { return err }

session, err := fg.Uploads.Sessions.Create(ctx, filegate.UploadSessionCreateRequest{
    Path:        "data/uploads/file.bin",
    Size:        info.Size(),
    Checksum:    checksum,
    SegmentSize: 8 << 20,
    OnConflict:  string(filegate.ConflictError),
})
if err != nil { return err }

for _, segment := range session.Segments {
    buf := make([]byte, segment.Size)
    if _, err := f.ReadAt(buf, segment.Offset); err != nil { return err }
    _, err := fg.Uploads.Sessions.PutSegment(ctx, filegate.UploadSessionPutSegmentRequest{
        SessionID: session.ID,
        Index:     segment.Index,
        Body:      bytes.NewReader(buf),
        Checksum:  segments.SHA256Bytes(buf),
    })
    if err != nil { return err }
}

out, err := fg.Uploads.Sessions.Commit(ctx, filegate.UploadSessionCommitRequest{
    SessionID: session.ID,
})
if err != nil { return err }
fmt.Println(out.Node.ID)
```

## Properties

- **Durable progress**: session metadata lives in Pebble; staged bytes live
  under the mount-local `.fg-uploads` namespace.
- **Out-of-order delivery**: segment indexes can arrive in any order.
- **Duplicate-safe**: a repeated segment PUT is a no-op when bytes match.
- **Explicit commit**: data is only published after final checksum
  verification and atomic replace/create.
- **Abortable**: `DELETE /v1/uploads/sessions/{id}` removes staged bytes for
  in-progress sessions.
- **Reserved staging**: `.fg-uploads` is not addressable through normal
  Filegate paths and is skipped by detection/rescan.
