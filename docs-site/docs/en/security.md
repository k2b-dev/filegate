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
expiry. Do not log URL paths at the reverse proxy. Rotate the token by replacing
the token file and restarting; this invalidates outstanding scoped URLs too.

Use TLS at the reverse proxy. Restrict direct daemon access to trusted networks.
CORS only controls browser access; it is not authorization. Configure exact
allowed origins for cross-origin direct transfers.

## Unix ownership

The backend can provide numeric `uid`, `gid`, `mode` and `dirMode` for uploads and
directory creation. Resolve user and group IDs in your application. The daemon
performs filesystem operations under its service account. When ownership is
omitted, new files belong to that account and overwrites preserve existing
ownership. See [direct transfers](uploads-downloads.md) for mode defaults.

Assigning files to other users requires permission to change ownership and to
read and write those files afterward. Configure the service account and Linux
capabilities for the required access. `CAP_CHOWN` alone does not grant access to
user-owned mode-0600 files. NFS `root_squash` may reject ownership changes even
for local root. Test the actual mount and service identity before relying on it.

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
