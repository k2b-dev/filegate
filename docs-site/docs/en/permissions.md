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

The service must retain read access to managed files and read/traverse access to
directories. Metadata operations open the target before changing it. If an ACL
removes the service's access, an operator may need to restore it outside Filegate.
Ownership and ACL updates can partially succeed before a later step fails;
inspect the resulting state before retrying.

## Configure a shared directory

The example creates a directory owned by UID 10001 and GID 20001, writable by
that group. Use your own application's numeric IDs. Create it under an existing
parent whose permissions are already appropriate.

Ownership and ACL requests are separate operations. Keep the directory private
until all requests succeed. If setup fails, inspect its current state before
retrying or exposing it to users.

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

await root.mkdir(path, { uid: 10001, gid: 20001, dirMode: "0700" });
await root.setACL(path, "default", defaults);
const directory = await root.setOwnership(path, { dirMode: "2770" });
const inherited = await root.getACL(path, "default");
console.log(directory.uid, directory.gid, directory.mode, inherited.entries);
```

Verify UID 10001, GID 20001, mode `2770` and the default entries before enabling
application access. The owner still has access during setup. The service needs
permission to finish configuring the directory after assigning its owner.

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
new files default to 0644 and directories request 0755 subject to the service
umask. With a default ACL, inherited file access is initially limited by 0666.
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
