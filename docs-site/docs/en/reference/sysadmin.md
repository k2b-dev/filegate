---
title: System administration reference
navTitle: System administration
section: Deep reference
order: 265
description: Detailed production operation, storage, backup, restore, upgrade, and recovery guidance.
tags: [reference, operations, recovery]
---

# Sysadmin Guide

This guide is for deploying, operating, and maintaining Filegate in production.

## 1. Installation options

- Package install: RPM/DEB with systemd unit. This is the recommended production path.
- Static binary: direct service management with your own unit.
- Container: useful for local evaluation, CI smoke tests, or container-standardized environments.

Target platform is Linux.

## 2. Configuration Loading

Resolution order:

1. `--config <path>`
2. `FILEGATE_CONFIG`
3. default candidates (`/etc/filegate/conf.yaml`, local `conf.yaml`, legacy names)
4. env overrides (`FILEGATE_*`)

Canonical package config path: `/etc/filegate/conf.yaml`.

Reference config template:

- [packaging/config/conf.yaml](https://github.com/ValentinKolb/filegate/blob/main/packaging/config/conf.yaml)

## 3. Required Settings and Durable State

Minimum explicit production configuration:

```yaml
storage:
  base_paths:
    - /var/lib/filegate/data
  index_path: /var/lib/filegate/index
  runtime_config_path: /var/lib/filegate/config
```

Set `auth.bearer_token` through a protected bootstrap file or environment. If
it is empty, Filegate generates a strong token on first start, stores it in the
runtime config store, and prints it once. Empty never means unauthenticated
REST, including in S3 deployments.

The three durable state classes have different recovery semantics:

| State | Default path | Authority | Recovery |
|---|---|---|---|
| File tree and `user.filegate.id` xattrs | `storage.base_paths` | File bytes, paths, stable IDs, and version blobs | Restore from a backup that preserves xattrs. |
| Runtime config store | `/var/lib/filegate/config` | Applied manifest, generated REST token, and S3 keys | Restore from backup; it is not reconstructable from the file tree. |
| Metadata index | `/var/lib/filegate/index` | Listings, lookup, and upload-session metadata | Restore only with the matching data snapshot, or rebuild offline. |

The bootstrap config at `/etc/filegate/conf.yaml` and any environment-backed
secrets are deployment inputs; back them up in their owning secret/config
system.

Strongly recommended explicit settings:

- `storage.index_path`
- `server.listen`, `server.public_url`, `server.cors.*`, `server.shutdown_timeout`
- `upload.max_*`
- `jobs.*`
- `detection.backend`, `detection.poll_interval`, and `detection.reconcile_interval`

## 4. systemd Service Operation

Unit file:

- [packaging/systemd/filegate.service](https://github.com/ValentinKolb/filegate/blob/main/packaging/systemd/filegate.service)

Typical commands:

```bash
sudo systemctl daemon-reload
sudo systemctl enable filegate
sudo systemctl start filegate
sudo systemctl status filegate
sudo journalctl -u filegate -f
```

The package installs the service without starting it. Configure its storage
paths and credentials before enabling it.

Package install provides `/usr/bin/fg`. It can also add the optional shell alias:

```bash
sudo FILEGATE_INSTALL_ALIAS_FG=1 dpkg -i ./dist/filegate_<version>_linux_amd64.deb
# or:
sudo FILEGATE_INSTALL_ALIAS_FG=1 rpm -Uvh ./dist/filegate-<version>-1.x86_64.rpm
```

Bash and Zsh get `alias fg='filegate'` in the invoking user's shell config. Other shells receive the snippet on stdout.

## 5. systemd Hardening and Base Path Access

The default unit uses strict hardening (`ProtectSystem=strict`).

Operational consequence:

- Filegate can only write to paths allowed by `ReadWritePaths`.
- If your `storage.base_paths` are outside allowed paths, writes will fail.

Recommended override for custom storage paths:

```bash
sudo systemctl edit filegate
```

```ini
[Service]
ReadWritePaths=/var/lib/filegate /var/log/filegate /srv/filegate/data
```

Then:

```bash
sudo systemctl daemon-reload
sudo systemctl restart filegate
```

## 6. Filesystem Choice

ext4, XFS, and btrfs are supported when the mounted roots are writable and
support user xattrs.

### ext4

- Stable default choice
- Predictable behavior under mixed workloads
- Simple operational model

### btrfs

Advantages for Filegate workloads:

- External-change ingestion can use `btrfs subvolume find-new` generation deltas instead of broad polling scans.
- Better snapshot/rollback workflows for data roots
- Can improve metadata-heavy workloads in some setups
- Useful tooling for filesystem-level observability and management

Tradeoffs:

- More operational complexity than ext4
- Requires experienced tuning/monitoring in production

If your team does not already operate btrfs confidently, start with ext4.

### XFS

- Supported with polling detection.
- User xattrs must be enabled and preserved by backup tooling.
- Reflink availability depends on how the filesystem was created; Filegate
  probes `FICLONE` instead of trusting the filesystem name.

### Practical impact for Filegate detectors

- On btrfs, Filegate can process foreign filesystem activity (NFS writers, local tools, sidecar processes) much more efficiently by reading transid/generation deltas.
- On ext4/xfs, Filegate cannot use `find-new`; detector fallback is polling-based and must repeatedly scan/check directory trees and files.
- As tree size and churn increase, ext4/xfs polling overhead can grow sharply compared to btrfs delta-based ingestion.

Versioning is not selected by filesystem name. `versioning.enabled=auto`
enables versioning only when every configured mount passes the real reflink
probe. `on` enables it on all mounts and falls back to byte copies where
reflinks are unavailable. The System API reports the effective `reflink`,
`mixed`, `byte-copy`, or `disabled` mode and its reason.

## 7. Detector Backend Strategy

`detection.backend` options:

- `auto`: preferred default (auto-select backend)
- `poll`: periodic polling
- `btrfs`: btrfs-specific path

Behavior model:

- HTTP writes are immediately reflected in index reads.
- External filesystem changes are eventual-consistent through detector sync.
- Unknown detector scopes can trigger mount-scoped rescan behavior.
- A full reconciliation runs every `detection.reconcile_interval` (default
  `24h`) as a simple safety net for missed events; `0s` disables it.

Efficiency note:

- `detection.backend=btrfs` is a major optimization when roots are on btrfs subvolumes.
- `detection.backend=poll` is functionally correct but materially heavier on very large trees.
- The standard distroless container has no `btrfs` CLI, so `auto` selects
  `poll`; the System page reports the effective backend and reason.

Do not place nested btrfs subvolumes inside a root watched by the btrfs
detector. Externally deleting a nested subvolume can stop generation processing
for the parent until Filegate restarts. Use separate configured roots, or
restart Filegate and verify `/v1/health` plus detector cycles in
`/v1/system/runtime` after such a deletion.

## 8. Capacity and Sizing

Main levers:

- `cache.path_cache_size`
- `jobs.workers`, `jobs.queue_size`
- `jobs.thumbnail_*`
- `upload.max_chunk_bytes`, `upload.max_upload_bytes`, `upload.max_session_upload_bytes`
- `upload.max_concurrent_segment_writes`, `upload.min_free_bytes`

`upload.max_session_upload_bytes` must be greater than or equal to
`upload.max_chunk_bytes`; startup rejects the config otherwise.

Host-level levers:

- `LimitNOFILE` (unit already sets high value)
- storage throughput and latency
- CPU core count and RAM

Rule of thumb:

- prioritize metadata latency first (index/cache)
- then tune write throughput (chunk size, queue sizing, storage)

## 9. Security Checklist

- Keep Filegate non-public, reachable only from trusted backend tier.
- Use strong bearer token, rotate regularly.
- Restrict incoming source IPs via firewall/security groups.
- Keep base paths explicit and minimal.
- Run as dedicated service user (`filegate`).
- Audit systemd overrides after upgrades.
- Terminate TLS at a reverse proxy; Filegate listeners are cleartext.
- Apply REST rate limits at the proxy. Filegate has no built-in REST request
  limiter; S3 has optional per-key limits.
- Treat the Admin app as full Filegate authority and keep its bearer token
  server-side.
- Run one daemon only for each runtime/index store and writable mount set.

## 10. Routine Operations

Liveness, readiness, and diagnostics:

```bash
curl -fsS http://127.0.0.1:8080/health
curl -fsS -H 'Authorization: Bearer <token>' http://127.0.0.1:8080/v1/health
curl -fsS -H 'Authorization: Bearer <token>' http://127.0.0.1:8080/v1/system/info
```

- `/health` is an unauthenticated liveness check and only proves that the
  process accepts HTTP.
- `/v1/health` is the readiness signal: it checks the index, detector
  staleness, and mount reachability. `fail` returns 503; `degraded` returns 200.
- `/v1/system/info` performs the slower write/xattr/reflink mount probes and
  shows effective detector/versioning choices. Read it after start and
  occasionally, not as a frequent probe.
- `/v1/system/runtime` is safe to poll for live queues, detector state, cache
  ratios, and upload sessions. `/v1/stats` is capacity data, not readiness.

Index ops:

```bash
fg index stats --config /etc/filegate/conf.yaml
fg index rescan --config /etc/filegate/conf.yaml
fg index rescan --new --config /etc/filegate/conf.yaml
fg index rescan --new --skip-backup --config /etc/filegate/conf.yaml
fg health --config /etc/filegate/conf.yaml
fg status --config /etc/filegate/conf.yaml
```

Important for `index rescan --new`:

- Stop `filegate` first.
- The command runs offline and exits with an error if index files are in use.
- By default it creates a timestamped backup of the previous index directory.
- Use `--skip-backup` only when you explicitly do not want a rollback artifact.

Recommended cadence:

- poll authenticated readiness and error logs continuously
- review stats trends daily (index size, cache usage, disk usage)
- run `index rescan --new` for severe index corruption scenarios (daemon stopped)

## 11. Backup and Restore

Create a point-in-time backup with the service stopped, or use coordinated
filesystem snapshots that cover every data root and the runtime store. Do not
copy a live Pebble directory independently from the file tree.

Back up:

1. Every `storage.base_paths` tree, including `.fg-versions`, `.fg-uploads`,
   ownership, modes, and `user.filegate.id` xattrs.
2. `storage.runtime_config_path` because it contains desired state and generated
   credentials.
3. `/etc/filegate/conf.yaml`, environment/secret-manager inputs, reverse-proxy
   config, and Admin secrets in their normal deployment backup.
4. Optionally `storage.index_path`, but only from the same point in time. The
   index speeds recovery; it is not a substitute for data/runtime backups.

For file-copy backups, use tooling that preserves xattrs, for example
`rsync -aHAX --numeric-ids`. Verify a restored sample with
`getfattr -n user.filegate.id <file>` before starting Filegate.

Restore in this order:

1. Keep every Filegate instance stopped.
2. Restore data roots and their xattrs.
3. Restore the runtime config store and bootstrap/secret inputs.
4. Restore an index only when it belongs to the same backup point. Otherwise
   leave the daemon stopped and run `fg index rescan --new --config ...`.
5. Start exactly one instance. Verify `/v1/health`, inspect
   `/v1/system/info`, resolve a known ID, and perform a controlled read/write.

Rebuilding the index discards in-progress upload-session metadata; staged
`.fg-uploads` bytes alone cannot resume those sessions. Recent activity is an
in-memory ring and is never restored.

## 12. Upgrade and Rollback

Before upgrade:

- make a verified backup using the procedure above
- record the current binary/package/image version and manifest revision
- validate package signature/source in your normal process

Upgrade:

- stop `filegate` before installing a `.deb`/`.rpm` upgrade
- install the new package or container image
- start service
- verify `/health`, authenticated `/v1/health`, `/v1/system/info`, and key
  read/write flows

Package upgrades fail before replacing files when `filegate.service` is still active. The package script prints the stop instruction and leaves the existing install in place.

Rollback:

- stop Filegate and restore the previous package or image
- restore data, runtime store, and matching index together only if the failed
  upgrade changed durable state; otherwise keep the current state
- start one instance and repeat the readiness, ID-resolution, and read/write
  checks

There is no automatic schema downgrade promise. Test forward and rollback paths
with a copy of production state before an upgrade that changes on-disk formats.

## 13. Operating requirements

- Run one daemon per writable mount set, runtime store, and Pebble index.
- The REST bearer token grants full file and configuration authority for the
  Filegate instance.
- No multi-file transactions or cross-request point-in-time snapshot. Each
  successful write publishes its own result atomically.
- External writers are eventually indexed; the reconciliation interval bounds
  missed detector events when enabled.
- Nested btrfs subvolumes are not supported inside a watched btrfs root; an
  external nested-subvolume deletion requires a Filegate restart.
- S3 is path-style and does not expose S3 object versioning. S3 overwrites can
  feed Filegate's internal version capture, which is administered through REST.
- No durable audit log and no OpenTelemetry traces. Export logs/metrics and
  activity to external systems if those are requirements.
- The published Filegate container targets Linux AMD64. Packages cover Linux
  AMD64 and ARM64. Deploy the source-shipped Admin app from a pinned commit.

## 14. Troubleshooting Quick Table

- `401 unauthorized`: wrong/missing bearer token.
- `permission denied` on write: systemd `ReadWritePaths` mismatch.
- high metadata latency: cache too small, storage pressure, or detector churn.
- delayed external updates: detector backend/poll settings too conservative.
- chunk finalize failures: checksum mismatch or incomplete upload set.
- `507 insufficient storage`: upload guard (`upload.min_free_bytes`) prevented writes near full disk.
- index startup error with malformed/corrupt Pebble files: stop daemon, run `fg index rescan --new`, start daemon.
