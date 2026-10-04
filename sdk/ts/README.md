# @k2b/filegate

Root-scoped TypeScript client for Filegate. Use it on trusted backends; give
browsers scoped direct URLs and the token-free `@k2b/filegate/utils` helpers.

```ts
import { Filegate } from "@k2b/filegate";
const files = new Filegate({ baseUrl: "https://files.example.org", token });
const upload = await files.root("documents").directUpload("notes.txt", 5);
// Authorized browser: fetch(upload.url, { method: "PUT", body: "hello" })
```

Read `root.info()` before relying on file IDs. `stableIds` is true only when the
root has indexing enabled and `managed: true`, which requires exclusive Filegate
writers. These roots expose IDs for files and directories; new IDs use UUIDv7.
Store the root name with each ID; other roots omit public file IDs.

Use `root.resolve(id)` for the current Node, `root.contentByIDRaw(id, signal?)`
for current bytes, or `root.directDownloadByID(id, { expiresIn?, fileName? })`
for a browser download lease. Raw content methods preserve HTTP errors: check
the response status. Authorize access in your backend before using any ID.

IDs persist through same-root moves and content overwrites. Copies and cross-root
moves use the destination's ID: a new target gets a new ID, while overwriting a
target retains its existing ID. Deleting and recreating a path creates a new ID.
A download lease binds the file ID and its path at issuance. If the file moves,
issue a new lease by ID. The old lease cannot download another file that reuses
the path.

See the [TypeScript guide](https://filegate.dev/docs/en/ts-sdk),
[transfer guide](https://filegate.dev/docs/en/uploads-downloads) and
[portable skill](https://github.com/k2b-dev/filegate/tree/main/skills/filegate).
