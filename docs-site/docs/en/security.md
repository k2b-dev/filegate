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
FreeIPA and local Cloud users are application concepts, not Filegate identities.

Keep the bearer token out of browsers. Scoped upload/download/session URLs are
bearer capabilities: anyone holding one can perform its bound operation until
expiry. Do not log URL paths at the reverse proxy. Rotate the token by replacing
the token file and restarting; this invalidates outstanding scoped URLs too.

Use TLS at the reverse proxy. Restrict direct daemon access to trusted networks.
CORS only controls browser access; it is not authorization. Configure exact
allowed origins for cross-origin direct transfers.

## Unix ownership

The backend can provide numeric `uid`, `gid`, `mode` and `dirMode`. Filegate applies
them to the published file and newly created parents. It does not discover users
or groups, switch identities per request, or implement ACL inheritance policies.
Cloud-only storage can omit ownership and use the daemon account.

A normal service account cannot chown files to arbitrary users. For FreeIPA-owned
files, provide a service privilege arrangement that can both chown and later
read/write those files. This may require narrowly configured Linux capabilities
or a deliberately privileged daemon. `CAP_CHOWN` alone does not grant access to
user-owned mode-0600 files. NFS `root_squash` may reject these operations even for
local root. Test the actual mount and service identity before relying on it.

The packaged service defaults to an unprivileged `filegate` account. Granting
additional capabilities or changing it to root is an explicit operator decision.

## Filesystem boundaries

Paths are relative to a named root. Parent traversal, symbolic links and reserved
`.filegate` paths are rejected. File access uses directory descriptors and
`O_NOFOLLOW`; publication uses atomic rename followed by directory fsync.
Indexed regular files must not have hard links. Stable xattrs are not credentials:
copied IDs on different inodes are reissued so they cannot inherit another file's
history. Do not expose or modify private version/staging directories externally.

Keep the root directory itself under daemon/operator control. External users may
write in their assigned subdirectories, but must not be able to rename or replace
the root-level `.filegate` directory or its `LOCK` file. The root lock assumes
that private namespace remains in place.

Root storage must be trusted infrastructure. Do not place device files or
untrusted bind mounts inside a root. External writers bypass Filegate versioning,
and no index setting can capture their previous bytes retroactively.
