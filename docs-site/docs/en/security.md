---
title: Security and ownership
section: Operate
order: 30
description: Authentication boundaries, scoped URLs and Linux service permissions.
---

# Security and ownership

The bearer token authorizes all roots and administrative operations. Filegate
does not authenticate individual application users. The application backend must
check each user's access before making a request or issuing a scoped URL.

Keep the bearer token out of browsers. Scoped upload/download/session URLs are
bearer capabilities: anyone holding one can perform its bound operation until
expiry. Leases default to 60 seconds and are limited to 300 seconds. Expiry is
checked when a request starts; an accepted download can finish afterward. Leases
are reusable and have no individual revocation or authorization callback.
Do not log URL paths at the reverse proxy. Rotate the token by replacing
the token file and restarting; this invalidates outstanding scoped URLs too.

Use TLS at the reverse proxy. Restrict direct daemon access to trusted networks.
CORS only controls browser access; it is not authorization. Configure exact
allowed origins for cross-origin direct transfers.

## Controlled uploads and archive selections

Session leases permit status queries and segment uploads, plus abort when the
backend enables it. They never permit commit or lease renewal. Reauthorize the
user and recheck the upload budget on your backend before commit. For public
inboxes, use sessions for every file size, unique backend-selected paths and
`onConflict: "error"`. A direct PUT publishes without another backend check.

ZIP leases bind the complete selection manifest. A directory selection grants
access to its whole current subtree, including files created after issuance.
Authorize that scope before creating the lease. Path-based file download leases
also serve the contents found at the authorized path when used.

## Unix ownership

The backend can provide numeric `uid`, `gid`, `mode` and `dirMode` for uploads and
directory creation. Resolve user and group IDs in your application. The daemon
performs filesystem operations under its service account. When ownership is
omitted, new files use that account as owner and inherit the group of a setgid
parent. Overwrites preserve existing ownership and access ACLs. Use `dirMode`
for directory modes, including setgid (`"2770"`). See
[permissions and ACLs](/docs/en/permissions) for inheritance and shared directories.

Assigning files to other users requires permission to change ownership and to
read and write those files afterward. Configure the service account and Linux
capabilities for the required access. `CAP_CHOWN` alone does not grant access to
user-owned mode-0600 files. NFS `root_squash` may reject ownership changes even
for local root. POSIX ACL changes also require filesystem support and sufficient
privileges. Filegate does not translate NFSv4 ACLs. Test the actual mount and
service identity before relying on ownership or ACL changes.

The packaged service defaults to an unprivileged `filegate` account. Granting
additional capabilities or changing it to root is an explicit operator decision.

## Filesystem boundaries

Paths are relative to a named root. Parent traversal, symbolic links and reserved
`.filegate` paths are rejected. Indexed regular files must not have hard links.
File IDs identify files; access still requires authorization from the application.
Copying a file's ID attribute to another file does not transfer its identity or
history. Do not expose or modify private version and staging directories externally.

Keep the root directory itself under daemon/operator control. External users may
write in their assigned subdirectories, but must not be able to rename or replace
the root-level `.filegate` directory or its `LOCK` file.

Root storage must be trusted infrastructure. Do not place device files or
untrusted bind mounts inside a root. External writers bypass Filegate versioning,
and no index setting can capture their previous bytes retroactively.
