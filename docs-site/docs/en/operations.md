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

## Enable Unix execution

The packaged service runs as `filegate:filegate`. Roots with `execution: true`
require a Linux daemon running as root so it can start operations with the exact
requested UID/GID and supplementary groups. UID 0 is not an accepted execution
identity. Enabling the setting with the default service account fails startup.

For a new deployment that needs this feature, configure a systemd override with
`sudo systemctl edit filegate`:

```ini
[Service]
User=root
Group=root
```

Keep `NoNewPrivileges=true` and the packaged filesystem protections. Add your
root paths to `ReadWritePaths` when they are outside `/srv/filegate`. The service
must retain the privileges needed to change credentials, prepare ownership and
access its private state; a custom capability restriction must account for these
operations. `CAP_CHOWN` alone is insufficient.

Set `execution: true` on each applicable root, validate the configuration and
restart the service. Read root information with the unscoped backend client:
`execution: true` advertises the setting. Verify an allowed read and a denied
read using the intended numeric identity before enabling application access.
The setting permits scoped execution; it does not force every backend request
to supply an identity.

For an existing installation, stop the daemon and preserve a backup before
changing its service identity. Its private `.filegate` directories must be owned
by the new daemon UID with mode 0700; state must remain accessible to the daemon.
Adjust only private service data as needed. Do not recursively change ownership
or ACLs on users' files. Restarting with mismatched private ownership fails.

Execution work is bounded. Capacity exhaustion returns `503 execution_capacity`
with `Retry-After: 1`. Credential setup failures return an error rather than
falling back to privileged file access. The Linux `fs.suid_dumpable` setting must
be 0 or 2; value 1 is rejected. Container deployments also need an explicitly
privileged service identity and compatible filesystem,
capability and security settings; the default non-root image does not enable it.

## Dashboard data

`GET /v1/system` returns build identity, uptime, readiness and the latest periodic
maintenance error. `GET /v1/roots` or `GET /v1/roots/{root}` returns:

- Whether Unix execution identities (`execution`) and exclusive-writer
  conditional publication (`managed`) are enabled.
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
including data outside configured roots. Stats are invalidated when mutations change totals; refresh them when the
dashboard needs a new measurement. Inspect `complete`, `freshness`, `source`
and the `started`/`completed` interval. Unknown or partial values are not exact
quotas. See [directory observations](/docs/en/browsing#read-recursive-totals).

## Upload storage and receipt retention

Sessions accept uploads for 24 hours. Assembly temporarily stores the received
segments and a complete file, requiring roughly twice the upload size before
existing files, versions and parallel transfers are counted. Reserve enough disk
space and enforce aggregate quotas in the application.

Committed, aborted and expired session records remain available for seven days
after their terminal transition; expiry retention starts at the session deadline.
Maintenance expires sessions and removes obsolete records every five minutes.
An application should reconcile reserved upload budgets within the retention
window. A missing receipt afterward does not identify the upload outcome.

## Back up and recover

Stop Filegate and coordinate external writers before a consistent backup. Save:

1. Every root, including its private `.filegate` directory, ownership, modes, ACLs and xattrs.
2. The complete `state_dir`.
3. Static configuration and the token through your secret backup process.

Use filesystem or VM snapshots that preserve inode identity for a full rollback.
Restoring ordinary file copies onto new inodes assigns new file IDs, even when
xattrs are preserved. This does not restore the original version histories.
To recover current contents from file copies, use a new root and state directory.
Keep the original backup intact, including its identity records.

After an unclean stop, start with the same roots and state. Filegate recovers
pending local publications and namespace changes before serving requests.
Cross-root move source deletion requires an explicit backend resume. Inspect its
destination-root receipt; pending receipts do not expire automatically. If recovery fails,
startup stops with an error. Preserve the state and logs for diagnosis.

## Troubleshoot

| Symptom | Check |
| --- | --- |
| Service will not start | Configuration validation, root existence, token permissions, private `.filegate` permissions, state lock and journal logs. |
| Chown or ACL change returns 403 | Service privileges, file ownership and NFS root-squash policy; see [security](/docs/en/security). |
| ACL request returns 501 | The actual filesystem or mount does not expose POSIX ACLs. NFSv4 ACLs are not translated. |
| Group cannot write a new file | Check the parent default ACL, child access ACL and mask, and the writer's requested mode; see [permissions](/docs/en/permissions). |
| External files missing from search | Indexed roots need `filegate rebuild ROOT`. Index-free listings read the filesystem directly. |
| Listing/search/root-stats refresh returns 413 | Narrow the query or raise `maxEntries` within its documented limit. No globally sorted partial page is returned. |
| Browse returns 409 `cursor_invalid` | Discard accumulated pages and restart with the same query. |
| Upload returns 412 | Read the current managed revision and reconcile the edit; the upload condition failed without publication. |
| Move remains `source_pending` | Inspect the receipt and both paths before resume or abandon; see [transfer recovery](/docs/en/transfers#recover-a-cross-root-move). |
| Direct URL returns 401 | Expiry, token rotation or a modified signed URL. Mint a fresh URL. |
| Upload returns 409 | File conflict, changed duplicate segment, incomplete session or disabled feature. |
| Transfer returns 503 | Upload or archive stream capacity reached; retry with backoff. |
| Session is `expired` | Create a new session; renewing a lease cannot extend the 24-hour session lifetime. |
| Commit response is lost | Query the authenticated session endpoint or retry commit within the receipt retention window. |
| Version capture fails | Available storage, filesystem errors and private storage permissions. The previous file remains current. |
