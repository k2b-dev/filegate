---
title: Operations
section: Operate
order: 20
description: Run systemd, rebuild indexes, inspect dashboard state and back up safely.
---

# Operations

Run one active daemon per state directory and writable root set. NFS deployments
require server-side advisory locking; see the
[Linux flock documentation](https://man7.org/linux/man-pages/man2/flock.2.html).
Restart the daemon to apply configuration changes. Administrative CLI commands
call the daemon at `server.public_url` using the token file.

```sh
filegate validate
filegate status
filegate roots
filegate rebuild documents
filegate stats documents
filegate prune documents
journalctl -u filegate -f
```

`rebuild`, `stats` and `prune` are explicit maintenance operations. A rebuild
pauses Filegate mutations in that root. Other roots stay available. External
writers are outside this lock; coordinate them separately. Do not delete the
state directory to rebuild an index: it also contains authoritative version
metadata, identity claims and upload records.

## Dashboard data

`GET /v1/system` returns build identity, uptime, readiness and the latest periodic
maintenance error. `GET /v1/roots` or `GET /v1/roots/{root}` returns:

- Index enabled/running status, scanned count, last successful rebuild, duration
  and error.
- Optional recursive file count, directory count and current logical bytes,
  with source and timestamp. Missing totals are `null`. Explicit stats refresh
  is bounded; ordinary GET requests never recursively scan the filesystem.
- Filesystem capacity, available bytes and device identifier. Roots sharing a
  device share capacity; do not add their capacity numbers together.
- Version count, logical version bytes, cooldown and retention settings.
- Active upload sessions and their received staging bytes.

Version bytes report logical file sizes. Actual disk usage depends on filesystem
allocation and reflink support. Free space covers the entire filesystem,
including data outside configured roots. Stats are invalidated when mutations
change totals; refresh them when the dashboard needs a new measurement.

## Back up and recover

Stop Filegate and coordinate external writers before a consistent backup. Save:

1. Every root, including its private `.filegate` directory and xattrs.
2. The complete `state_dir`.
3. Static configuration and the token through your secret backup process.

Use filesystem or VM snapshots that preserve inode identity for a full rollback.
Restoring ordinary file copies onto new inodes assigns new file IDs, even when
xattrs are preserved. This does not restore the original version histories.
To recover current contents from file copies, use a new root and state directory.
Keep the original backup intact, including its identity records.

After an unclean stop, start with the same roots and state. Filegate recovers
pending writes, moves and deletions before serving requests. If recovery fails,
startup stops with an error. Preserve the state and logs for diagnosis.

## Troubleshoot

| Symptom | Check |
| --- | --- |
| Service will not start | Configuration validation, root existence, token permissions, private `.filegate` permissions, state lock and journal logs. |
| Chown returns forbidden | Daemon privilege and NFS root-squash policy; see [security](security.md). |
| External files missing from search | Indexed roots need `filegate rebuild ROOT`. Index-free listings read the filesystem directly. |
| Search/stats returns 413 | Raise the explicit `maxEntries` budget or narrow the operation; the server did not complete the scan. |
| Direct URL returns 401 | Expiry, token rotation or a modified signed URL. Mint a fresh URL. |
| Upload returns 409 | File conflict, changed duplicate segment, incomplete session or disabled feature. |
| Upload returns 503 | Concurrent upload capacity reached; retry with backoff. |
| Version capture fails | Available storage, filesystem errors and private storage permissions. The previous file remains current. |
