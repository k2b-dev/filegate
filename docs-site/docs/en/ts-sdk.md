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
instead, suitable for an authorized browser. `createSession` returns a scoped
session URL for larger uploads. Import token-free browser helpers from
`@k2b/filegate/utils`: `DirectSession`, `putDirect`, `segments` and `sha256`.

## Root operations

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
