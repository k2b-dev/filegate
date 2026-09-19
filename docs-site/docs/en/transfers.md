---
title: Copy and move files
section: Use
order: 25
description: Destination conflicts, complete directory publication, historical copies and recoverable moves.
---

# Copy and move files

The backend requests server-side transfers after authorizing the source and
destination. `root.transfer(path, targetRoot, targetPath, options)` returns a
`TransferResult`. Copies and same-root moves return `state: "completed"` and the
actual destination in `node`, including any renamed path.

```ts
const copied = await root.transfer("report.pdf", root.name, "report.pdf", {
  onConflict: "rename",
});
console.log(copied.node?.path);
```

A same-root move uses native rename and preserves the source inode, ID, revision
and history. It accepts only `onConflict`; ownership, access ACL, metadata and
publication conditions are rejected rather than silently ignored. Set those
attributes through their explicit APIs when needed.

## Choose a conflict policy

| Policy | Existing target |
| --- | --- |
| `error` (default) | Return 409 without replacing it. |
| `rename` | Choose a free sibling with a random suffix; return its actual path. |
| `overwrite` | Replace a regular file with a regular file. Any directory target or directory source conflicts. |

Directories are never merged. Random suffixes contain 128 random bits, with a
bounded number of attempts; ordinary file extensions are retained where possible.
Names are not numbered sequentially. Directory copies are staged privately and
become visible as a complete tree. A failure does not publish a partial copied
tree, but any newly created destination ancestors can remain. Tree copies are
bounded to 100000 entries and depth 128.

Copying a file or directory onto its own path with `rename` duplicates it under
a new name. The default policy conflicts; overwriting the source itself is
invalid. A move onto its own path with `rename` renames it to a sibling; other
policies return 409. Copying or moving a directory into its own descendants is
invalid.

For native file-over-file moves, the source ID and history survive at the target;
the replaced target's ID and history are removed. File copies use destination
publication rules: a new file gets a new ID/history; an overwrite preserves the
destination identity and captures its previous contents according to versioning.
Copies can specify `ownership`, `accessACL` and revision `metadata` explicitly.

## Copy historical bytes to a new target

```ts
const copied = await root.copyVersion(
  "report.pdf", versionId, "documents", "recovered/report.pdf", {
    onConflict: "error",
    ownership: { uid: 10001, gid: 20001, mode: "0640" },
  },
);
console.log(copied.path);
```

The current source's bytes, modification time, ID and history remain unchanged.
The target follows normal publication and conflict rules; default behavior is an
error on an existing target. Use `accessACL` for an explicit complete destination
file ACL. The same root and exact source path are forbidden as the target for
every conflict policy. A missing version returns 404; disabled source versioning
returns 409. With a Unix execution identity, current-source read access is also
required.

## Recover a cross-root move

Both roots must have `managed: true`. Save a new UUID on the backend before
starting a cross-root move; it identifies that exact operation after a lost
response or process restart.

```ts
const id = crypto.randomUUID(); // Persist with the application operation first.
const result = await files.root("incoming").transfer(
  "report.pdf", "documents", "report.pdf", {
    move: true, id, onConflict: "error",
  },
);
const destination = files.root("documents");
if (result.state === "source_pending") {
  const status = await destination.transferStatus(id);
  console.log(status.state, status.node?.path);
}
```

The destination root owns the receipt. Use `transferStatus(id)`,
`resumeTransfer(id)` and `abandonTransfer(id)` on that root. A retry with the same
UUID must describe the same source, destination, options and execution identity;
reusing it for another request returns 409. Copies and same-root moves reject a
transfer ID.

| State | Meaning |
| --- | --- |
| `prepared` | The request is recorded; destination publication is not confirmed. |
| `source_pending` | The destination is published; source removal is not durably confirmed. The source may already be absent after a crash. |
| `completed` | Destination publication and source removal are recorded. |
| `abandoned` | Further source-removal intent is cancelled; current files remain untouched. An already removed source is not restored. |

A pending transfer returns HTTP 202. Treat only `completed` as a completed move.
The operation is not an atomic cross-root transaction. After a lost response,
query the known receipt ID instead of starting another move with a different ID.

Resume is an explicit backend action. It checks the recorded bindings and
conservative content/namespace generations for both roots. Even an unrelated
write in either root can prevent source deletion with `412 precondition_failed`;
the source is preserved. Inspect the files and abandon the old intent when it is
no longer safe to continue. Startup recovery does not automatically resume source
deletion. Bound Unix execution identities remain in effect for any resumed file
operation. Changing either root's `managed` setting also invalidates pending
source-deletion checks; an ordinary restart with unchanged settings does not.

If source deletion was already durably confirmed, resume can finish the receipt
even after managed mode or Unix execution is disabled. This only records the
completed outcome; it performs no further file operation or source deletion.

Each destination root permits at most 128 unresolved move receipts. Pending
receipts remain until completed or abandoned; terminal receipts are retained for
at least seven days. A retained terminal result is idempotent. Once a receipt is
removed, 404 does not prove that the move failed. External writers violate the
managed-root contract, so these safeguards do not provide an atomic NFS move.
