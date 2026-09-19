---
title: Permissions and ACLs
section: Use
order: 40
description: Set up shared directories with setgid and POSIX access and default ACLs.
---

# Permissions and ACLs

Use setgid and a default ACL to give newly created files the shared directory's
group and intended permissions. These settings apply to Filegate writes and to
external writers using the same filesystem. ACL operations work with or without
an index and do not create versions.

The application resolves identities to numeric UID/GID values and authorizes
users. Filegate requires sufficient operating-system privileges and a filesystem
that supports POSIX ACLs. It does not translate NFSv4 ACLs. Verify support on the
actual NFS mount; `root_squash` can deny changes even when the daemon runs as root.

Without an execution identity, the service must retain read access to managed
files and read/traverse access to directories. Metadata operations open the
target before changing it. If an ACL
removes the service's access, an operator may need to restore it outside Filegate.
Ownership and ACL updates can partially succeed before a later step fails;
inspect the resulting state before retrying.

## Unix execution identity

Enable `execution: true` on a root to let a trusted backend run file operations
with a numeric Unix identity. The default is false. This requires the explicit
[root-service setup](/docs/en/operations#enable-unix-execution); ordinary requests
without an execution identity continue to use the daemon account.

Execution identity selects **whose filesystem permissions apply**. `ownership`
selects **who owns the resulting file**. The backend supplies both when needed;
Filegate does not resolve users or groups, or make application authorization
decisions.

```ts
const actor = files.root("shared").as({
  uid: 10001,
  gid: 20001,
  groups: [20002],
});
const lease = await actor.directDownload("teams/editors/report.pdf");
// Return only lease.url to the authorized browser.
```

The kernel checks directory traversal, content opens and live mutations using
the supplied UID, primary GID and supplementary groups. POSIX ACLs apply. A denied
operation fails; Filegate does not retry it under the daemon identity. Copies,
moves and ZIP selections use the same identity for every involved root. Each
root must have execution enabled.

File metadata can be read after successful traversal without granting content
read access. Live ownership, mode and ACL changes still require the execution
identity's permissions. Explicit `ownership` on a new upload, copied file or
new directory is a privileged provisioning instruction from the backend:
Filegate prepares it privately, then publishes it with the execution identity's
destination permissions. Without an ownership override, new objects use that
identity's UID/GID, with setgid-group and default-ACL inheritance from the parent.
Overwrites preserve the previous ownership and access ACL. Newly created parent
directories use the same private preparation and publication rules.

Leases bind the identity at issuance. Upload sessions store it at creation;
renewal and commit cannot replace it. Browser requests use only the lease and
must not send an execution header. Historical content requires read access to
the current file with the matching identity; Filegate does not store historical
ACLs. Thumbnail generation also opens its source under the bound identity.

Observe these native filesystem semantics:

- Replacing a directory entry checks write/search permission on its parent and
  sticky-directory rules. A read-only destination file alone does not prevent
  replacement when those directory permissions allow it. Moving a directory
  between parents also requires write permission on the moved directory.
- Recursive deletion removes the selected entry through a private quarantine.
  Authorization is for removing that entry from its parent, not a recursive
  `rm` permission check on every descendant. Filegate cleans up the detached
  tree and its history privately.
- Directory copies are prepared privately and published as a complete tree. A
  ZIP read failure aborts the stream; unreadable entries are never silently skipped to produce a
  successful archive. Treat an incomplete ZIP as a failed download.
- Changing permissions does not revoke an already-open file descriptor.
  Lease expiry prevents new requests; an accepted transfer can finish afterward.

Search, root dashboards, index maintenance, stats and pruning do not accept an
execution identity. Use an unscoped backend client for those administrative
operations; their results are not filtered by a user's Unix rights.

Numeric identities use the permissions exposed by the actual filesystem or NFS
mount. They do not obtain Kerberos credentials or bypass export policies such as
`root_squash`. Verify the intended identities against the actual deployment.

## Configure a shared directory

The example creates a directory owned by UID 10001 and GID 20001, writable by
that group. Use your own application's numeric IDs. Create it under an existing
parent whose permissions are already appropriate.

Create the directory and its permissions in one request. Its parent must already
exist. Filegate prepares the directory privately and publishes it only after the
requested ownership and ACLs are applied. An existing target returns 409 and is
not changed.

```ts
import type { ACL } from "@k2b/filegate";

const root = files.root("shared");
const path = "teams/editors";
const defaults: ACL = {
  entries: [
    { tag: "owner", permissions: "rwx" },
    { tag: "owningGroup", permissions: "rwx" },
    { tag: "other", permissions: "---" },
  ],
};

const directory = await root.mkdir(path, {
  ownership: { uid: 10001, gid: 20001, dirMode: "2770" },
  acl: { default: defaults },
});
const inherited = await root.getACL(path, "default");
console.log(directory.uid, directory.gid, directory.mode, inherited.entries);
```

Verify UID 10001, GID 20001, mode `2770` and the default entries before enabling
application access. The service needs permission to finish configuring the
directory after assigning its owner. You can also supply `acl.access` to set the
access ACL in the same request. An explicit `dirMode` sets the final effective
access permissions, including the ACL mask; named entries remain present. The
default ACL is independent of this mode.

Creation requires the destination and private staging area to share a filesystem.
An additional mount inside a root can prevent publication. After a lost response,
read the target state before retrying; a successful publication may already exist.

During setup, prevent external writers from renaming or replacing the parent or
changing its ownership, mode or ACLs. Inheritance uses the parent permissions read
when setup starts; Filegate does not coordinate these changes with NFS writers.

Without explicit overrides, new files inherit GID 20001 and group read/write
access. New subdirectories also inherit setgid and the default ACL. Ordinary
files do not inherit execute permission. An external program that explicitly
creates a file with `0600` can restrict inherited access: a default ACL provides
inheritance, not a mandatory minimum permission policy.

## Read and replace ACLs

An **access ACL** controls access to the current file or directory. A **default
ACL** is a directory template for future children; it does not grant access to
the directory itself. Changing or removing it does not update existing children.

`getACL(path, "access")` returns the owner, owning-group and other entries even
when no extended ACL is stored. `getACL(path, "default")` returns `{ entries: [] }`
when a supported directory has no default ACL. Unsupported ACLs return an error,
not an empty result.

`setACL` replaces exactly the requested scope. To add a named group, read the
current ACL, retain the entries you need and send the complete replacement:

```ts
await root.setACL("teams/editors", "access", {
  entries: [
    { tag: "owner", permissions: "rwx" },
    { tag: "owningGroup", permissions: "rwx" },
    { tag: "group", id: 20002, permissions: "r-x" },
    { tag: "mask", permissions: "rwx" },
    { tag: "other", permissions: "---" },
  ],
});
```

Every nonempty ACL needs one `owner`, `owningGroup` and `other` entry. Named `user`
and `group` entries require a numeric `id` and an explicit `mask`. The mask limits
all named entries and the owning group; Filegate does not widen it automatically.
PUT accepts 3–256 entries; IDs range from 0 to 4294967294. An empty PUT is invalid;
use `clearDefaultACL` to remove a default ACL. Permissions use exactly three characters: `rwx`, `rw-`, `r-x`, `r--`, `-wx`, `-w-`,
`--x` or `---`. IDs must be unique within each named entry type.

Use `clearDefaultACL(path)` to remove default inheritance. To remove extended
access entries, replace the access ACL with only owner, owning-group and other.
An explicit access-ACL replacement can change the mode reported by `stat`.
Likewise, setting `mode` or `dirMode` changes the ACL's effective permissions,
including its mask when present.

## Rights across file operations

| Operation | Rights |
| --- | --- |
| New upload or copied file | Inherits destination default ACL and setgid group; explicit ownership overrides apply. |
| New directory or parent | Inherits destination default ACL and setgid; explicit `dirMode` overrides the mode. |
| Overwrite or restore | Preserves ownership, ordinary permission bits and access ACL unless explicitly changed; clears file setuid/setgid bits. |
| Same-root move | Preserves the existing inode's ownership, mode and ACLs. |
| UID/GID-only change | Preserves existing access ACL and directory mode, including setgid. |
| Default-ACL change | Affects future children only. |

Direct uploads and resumable commits use these same rules. Without a default ACL,
new files default to 0644 and directories to 0755. With a default ACL, inherited file access is initially limited by 0666.
An explicit file mode is applied afterward and can widen or restrict effective
ACL permissions. Directories inherit using 0777, or an explicitly requested
`dirMode`; an explicit mode also sets their final access permissions. Existing
parent directories are not modified. No permission operation recursively
corrects existing files.

Modes are octal strings. File `mode` accepts permissions up to `0777`; `dirMode`
also supports setgid, for example `"2770"`. Read responses include special bits.
Use `dirMode` when calling `setOwnership` on a directory.

## Verify on Linux

Operators can inspect the filesystem independently with `getfacl`:

```sh
getfacl -n /srv/shared/teams/editors
getfacl -n /srv/shared/teams/editors/example.txt
```

Check the owner/group IDs, setgid flag, access entries, default entries and any
`effective` permissions limited by a mask. Create a file and subdirectory through
Filegate and through the external writer, then test access using a group member's
identity. `getfacl` is an optional operator tool, not a daemon dependency.

HTTP 400 means invalid input, 403 means insufficient permissions and 501 means
POSIX ACLs are unsupported on the target filesystem or mount. For 403, inspect
service privileges, file ownership and the NFS export policy. Changing client
privileges alone does not override a server's `root_squash` policy.
