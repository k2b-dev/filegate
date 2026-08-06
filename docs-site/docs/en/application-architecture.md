---
title: Use Filegate in an app
navTitle: App architecture
section: Start
order: 25
description: Use Filegate behind an application server with direct signed upload and download URLs.
tags: [architecture, browser, uploads]
---

# Use Filegate in an app

This page is for application developers who want to use Filegate as the file layer for a cloud app, CMS, media library, backup UI, or internal file service.

In the common deployment, your application server owns users, permissions, billing, projects, and product rules. Filegate owns filesystem-backed file operations, stable IDs, indexed lookup, upload sessions, direct URLs, downloads, optional S3 access, versions, activity, and metrics.

## Recommended shape

Keep the Filegate bearer token on your server. Let browsers upload and download directly through scoped URLs or scoped upload-session tokens.

```txt
              user session / app auth
Browser  <---------------------------->  App server
   |                                          |
   | direct scoped upload/download URL       | Filegate bearer token
   v                                          v
Filegate REST API  ----------------->  Linux mount roots
      |                                  /srv/cloud/files
      v                                  /srv/cloud/photos
Metadata index
```

## Responsibilities

| Component | Scope | Responsibilities |
|---|---:|---|
| Browser | One user session | Select files, show progress, upload segments, download through scoped URLs. |
| App server | One application | Authenticate users, authorize file actions, mint Filegate sessions or signed URLs. |
| Filegate | One file service | Store files on Linux mounts, enforce Filegate token/scoped-token rules, index metadata, commit uploads. |
| Filesystem | One host or mounted volume | Durable file bytes and directory layout. |

## Direct browser upload

The browser asks your app server for permission to upload. The app server checks its own rules, then creates Filegate upload sessions with direct tokens. The browser uploads segments to Filegate without sending bytes through the app server.

```txt
Browser              App server                 Filegate
   |  upload request     |                         |
   |-------------------->|                         |
   |                     | authorize user/action   |
   |                     |------------------------>|
   |                     | create direct sessions  |
   |                     |<------------------------|
   |  scoped sessions    |                         |
   |<--------------------|                         |
   |  PUT segments directly                       |
   |--------------------------------------------->|
   |  commit session                              |
   |--------------------------------------------->|
   |  final node                                  |
   |<---------------------------------------------|
```

This keeps application servers out of the transfer path while preserving application-level authorization.

## Direct browser download

For downloads, the browser asks your app server for a file. The app server authorizes the user and asks Filegate for a scoped download URL. The browser then downloads directly from Filegate.

```txt
Browser              App server                 Filegate
   |  download request   |                         |
   |-------------------->|                         |
   |                     | authorize user/action   |
   |                     |------------------------>|
   |                     | mint direct URL         |
   |                     |<------------------------|
   |  scoped URL         |                         |
   |<--------------------|                         |
   |  GET direct URL                               |
   |--------------------------------------------->|
   |  file or tar stream                          |
   |<---------------------------------------------|
```

Directories download as tar streams. Files download as their stored bytes.

## Minimal backend flow

The app server endpoint receives an upload plan from the browser, validates the user and target path, then calls Filegate.

```ts
import {
  Filegate,
  type BrowserUploadAllowRequest,
  type BrowserUploadAllowResponse,
  type UploadSessionDirectRequest,
} from "@valentinkolb/filegate";

const filegate = new Filegate({
  baseUrl: process.env.FILEGATE_URL!,
  token: process.env.FILEGATE_TOKEN!,
});

export async function allowUploads(request: Request) {
  const user = await requireUser(request);
  const body = (await request.json()) as BrowserUploadAllowRequest;

  for (const upload of body.uploads) {
    await assertCanWrite(user, upload.path);
  }

  const direct: UploadSessionDirectRequest = {
    allow: ["putSegment", "status", "commit", "abort"],
  };
  const sessionInputs = body.uploads.filter((upload) => upload.kind === "session");
  const sessions = sessionInputs.length
    ? await filegate.uploads.sessions.createBatch({
        uploads: sessionInputs.map((upload) => ({
          path: upload.path,
          size: upload.size,
          checksum: upload.checksum!,
          segmentSize: upload.segmentSize!,
          contentType: upload.contentType,
          onConflict: body.onConflict === "overwrite" ? "overwrite" : "error",
          direct,
        })),
      })
    : { sessions: [] };

  let sessionIndex = 0;
  const response: BrowserUploadAllowResponse = {
    uploads: await Promise.all(
      body.uploads.map(async (upload) => {
        if (upload.kind === "direct") {
          const directUpload = await filegate.uploads.createDirectUploadURL({
            path: upload.path,
            maxBytes: upload.size,
            contentType: upload.contentType,
            onConflict: body.onConflict === "overwrite" ? "overwrite" : "error",
          });
          return { id: upload.id, ok: true, upload: { kind: "direct", direct: directUpload } };
        }

        const session = sessions.sessions[sessionIndex++];
        return { id: upload.id, ok: true, upload: { kind: "session", session } };
      }),
    ),
  };

  return Response.json(response);
}
```

The browser uses the returned direct session data and does not receive the Filegate bearer token.

## When to use S3 instead

Use the native REST/SDK flow for application-owned file UX, browser uploads, metadata panels, versions, and direct URLs. Use the S3 listener when the client already speaks S3, such as backup tools, migration jobs, or object-storage integrations.

Both surfaces write to the configured Linux mounts.

## Related pages

- [Uploads and downloads](/docs/en/uploads-downloads) documents transfer patterns and integrity rules.
- [TypeScript SDK](/docs/en/ts-sdk) documents the browser upload helper.
- [HTTP API](/docs/en/http-api) documents the REST route groups.
- [S3 compatibility](/docs/en/s3) documents path-style S3 access.
