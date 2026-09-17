---
title: Version history
section: Use
order: 30
description: Snapshot triggers, cooldown, metadata, pins and calendar retention.
---

# Version history

Enable versioning on an indexed root to preserve earlier file contents. Before
an overwrite, Filegate durably snapshots the existing bytes. If that required
snapshot fails, the overwrite fails and the old file remains current. Uploading
a new file does not create an automatic version. Rename, ownership and ACL changes
do not create versions either.

## Choose capture frequency

The default cooldown is one minute, measured from the last successful snapshot.
Skipped writes do not reset it. With a one-minute cooldown:

| Time | Write | Captured version |
| --- | --- | --- |
| 00:00 | A → B | A |
| 00:20 | B → C | none |
| 00:40 | C → D | none |
| 01:10 | D → E | D |

Manual snapshots always bypass cooldown. Restore first snapshots the current
bytes, also bypassing cooldown, then publishes the selected version's contents
and revision metadata. Restore preserves the current file's ownership, ordinary
permission bits and access ACL. It clears file setuid and setgid bits.
These rules apply equally to direct PUT and session commits; chunks never create
versions.

## Attach metadata and keep manual versions

`metadata` is an arbitrary JSON **object**, limited to 8192 bytes in its serialized
UTF-8 representation. Oversized values are rejected, never truncated. System
fields such as ID, creation time and size are separate.

```ts
await files.root("documents").snapshot("reports/annual.pdf", {
  pinned: true,
  metadata: { message: "Approved draft", actor: "user-42" },
});
```

Upload metadata describes the incoming current revision. When that revision is
later snapshotted, its metadata follows its bytes. A manual snapshot can supply
its own metadata; omitting it uses the current revision's metadata.

Pinned versions survive automatic retention and do not consume `keep.last`.
Explicit deletion and permanent file deletion still remove them. Updating a
version replaces its pin and metadata fields; send both values you want to keep.

## Download a historical version

After authorizing access, the backend can call
`root.directVersionDownload(path, versionId)` and give the returned URL to the
browser. GET and HEAD require no backend token; GET supports Range requests.
The lease binds the root, current file path and exact version. It expires after
60 seconds by default and does not prevent deletion or pruning. Moving the file
requires issuing a new lease at its new path. Backend streaming remains available
through `versionContentRaw(path, versionId)`.

See [downloads and previews](/docs/en/uploads-downloads#downloads-and-previews)
for direct URLs and error behavior.

## Prune calendar buckets

```yaml
keep:
  last: 10
  hourly: 24
  daily: 30
  weekly: 8
  monthly: 12
```

Retention keeps the union of the last unpinned versions and the newest unpinned
version in each requested UTC calendar bucket. Daily means calendar days, weekly
means Monday-based weeks, and monthly means calendar months. The current bucket
counts. Pins are retained independently. A version selected by multiple rules is
stored once. A missing or zero tier retains nothing for that tier.

If the entire `keep` section is absent, defaults are `last: 10`, `daily: 30`,
and `monthly: 12`. An explicit `keep: {}` retains only pinned versions.

Retention selects existing snapshots; it does not create scheduled backups.
The daemon prunes every five minutes, and `filegate prune ROOT` runs a round
explicitly. Snapshot bytes are reflinked when supported on the underlying
filesystem, otherwise copied. Reported version bytes are logical sizes, not
additional allocated disk usage.

## Moves, copies and deletion

An API rename within the same root preserves the ID and history. Copies receive
new IDs and begin without history. A cross-root move copies the content and then
permanently removes the source and its history. It does not transfer history.
An application trash folder can use a same-root move to preserve history.
Permanent recursive deletion also deletes histories below that directory.
