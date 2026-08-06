---
title: Operations
navTitle: Operations
section: Use Filegate
order: 50
description: Operate Filegate with systemd, health checks, index rescans, activity inspection, and safe shutdown.
tags: [operations, systemd, index]
---

# Operations

This page is for operators running Filegate as a service.

Use the [Admin UI](/docs/en/admin) for browser-based file operations, runtime metrics, activity inspection, and index rescans.

## Service lifecycle

| Task | Scope | Command |
|---|---:|---|
| Start service | Host service | `sudo systemctl start filegate` |
| Stop service | Host service | `sudo systemctl stop filegate` |
| Enable on boot | Host service | `sudo systemctl enable filegate` |
| Read status | Host service | `sudo systemctl status filegate` |
| Reload unit files | Host systemd | `sudo systemctl daemon-reload` |

Bootstrap YAML and environment changes need a restart. Manifest runtime keys
activate immediately after `fg config apply`; static keys are stored as desired
state and appear in `restartRequired` until the service restarts.

## Health and status

| Signal | Scope | Command | Meaning |
|---|---:|---|---|
| Liveness | Service listener | `curl -fsS http://127.0.0.1:8080/health` | Process accepts HTTP requests; checks no dependency. |
| Readiness | Service dependencies | `curl -fsS -H 'Authorization: Bearer …' http://127.0.0.1:8080/v1/health` | Checks index, detector staleness, and mount reachability. `fail` returns 503. |
| Storage capabilities | Mounts and effective config | `GET /v1/system/info` | Performs slower writable/xattr/reflink probes and explains effective detector/versioning modes. |
| Runtime counters | In-memory service state | `GET /v1/system/runtime` | Safe polling surface for queues, detector, caches, and upload sessions. |
| Local status | Config file and service | `fg status --config /etc/filegate/conf.yaml` | Reads local config and runtime health. |

Use `/v1/health` for readiness and `/health` for liveness. `/v1/stats` reports
capacity and counts; it is not a readiness check. Read `/v1/system/info` after
startup and after storage changes, not on a frequent probe interval.

## Index rescan

Use a rescan when files changed outside Filegate and the detector did not observe the change.

```sh
fg index rescan --config /etc/filegate/conf.yaml
```

The admin app exposes the same operation as a background action. Use the activity log to inspect whether the operation finished and how long it took.

## Activity inspection

`GET /v1/activity` returns recent in-memory activity records.

| Field | Scope | Meaning |
|---|---:|---|
| `operation` | One activity event | Operation name such as `node.mkdir`, `node.delete`, or `index.rescan`. |
| `actor.kind` | One activity event | Auth source: `bearer_token`, `s3_key`, `signed_url`, or `system`. |
| `actor.delegatedActor` | One activity event | Optional label from `X-Filegate-Actor`. |
| `target` | One activity event | Node, path, bucket, or service object affected by the operation. |
| `outcome` | One activity event | `succeeded`, `failed`, or `skipped`. Current HTTP and S3 handlers record `succeeded` or `failed`. |

The activity log is an in-process ring buffer. It is useful for operator introspection and admin UI feedback. It is not durable compliance storage.

## Safe shutdown

`server.shutdown_timeout` bounds graceful shutdown. Long multipart completes need enough time to concatenate parts, verify hashes, write Pebble metadata, fsync, and rename.

| Config | Scope | Default | Meaning |
|---|---:|---:|---|
| `server.shutdown_timeout` | Service process | `60s` | Maximum time to wait for in-flight HTTP handlers before force-closing listeners. |
| `server.write_timeout` | HTTP request | `5m` | Maximum time for a response write. |

## Storage pressure

Uploads can fail before writing when free space is below `upload.min_free_bytes`.

| Signal | Scope | Source | Meaning |
|---|---:|---|---|
| `GET /v1/stats.disks[]` | Filesystem device | REST stats | Used and total bytes for each backing device. |
| `filegate_mount_free_bytes` | Mount | Prometheus | Free bytes on the filesystem backing a mount. |
| `upload.min_free_bytes` | Service | Config | Minimum free bytes required before accepting uploads. |

## Backup

> **Stop every Filegate instance or use coordinated filesystem snapshots.** A
> copied live Pebble directory is not a consistent backup of a concurrently
> changing file tree.

Back up these state classes:

| State | Required | Recovery meaning |
|---|---:|---|
| Every configured data root, including `.fg-versions` and `.fg-uploads` | Yes | File bytes, paths, versions, ownership, and stable IDs. Preserve `user.filegate.id` xattrs. |
| `storage.runtime_config_path` | Yes | Applied manifest, generated REST token, and S3 credentials. Not rebuildable. |
| Bootstrap config and external secrets | Yes | Inputs needed before the runtime store can open. |
| `storage.index_path` | Optional | Speeds recovery only when captured with the same data point. Otherwise rebuild it. |

For file-copy backups, use tooling that preserves ownership, modes, hard links,
ACLs, and xattrs, such as `rsync -aHAX --numeric-ids`. Verify a restored sample
with `getfattr -n user.filegate.id <file>`.

## Restore

1. Keep all Filegate instances stopped.
2. Restore data roots and xattrs.
3. Restore the runtime config store and bootstrap/secret inputs.
4. Restore the index only from the same backup point. Otherwise run the offline
   rebuild:

   ```sh
   sudo fg index rescan --new --config /etc/filegate/conf.yaml
   ```

5. Start exactly one instance.
6. Verify authenticated `/v1/health`, inspect `/v1/system/info`, resolve a known
   stable ID, and perform a controlled read/write.

An index rebuild loses in-progress upload-session metadata even when staged
`.fg-uploads` bytes remain. The activity ring is in memory and never restored.

## Upgrade and rollback

Package upgrades are offline. The package preinstall refuses to replace files
while `filegate.service` is active.

1. Make and verify a backup.
2. Record the current package/image version and manifest revision.
3. Stop Filegate and install the new package or image.
4. Start one instance and run the restore verification checks above.

To roll back, stop the service and reinstall the previous package or image.
Restore data, runtime store, and matching index together only when the failed
upgrade changed durable state. There is no general on-disk schema downgrade
promise; rehearse upgrades and rollbacks on a copy of production state.

## Fixed production boundaries

- Filegate is single-node and single-tenant. Run exactly one daemon for an
  index/runtime-store pair and writable mount set; there is no replication,
  leader election, or shared Pebble mode.
- The REST bearer token grants full file and configuration authority. S3 keys
  are the only bucket-scoped credentials.
- Filegate publishes individual operations atomically but provides no multi-file
  transaction or cross-request point-in-time snapshot.
- External filesystem changes become visible eventually through detection and
  reconciliation. Raw external writes do not create automatic versions.
- Do not nest btrfs subvolumes inside a root watched by the btrfs detector.
  After an external nested-subvolume deletion, restart Filegate and verify
  detector cycles before relying on further external-change ingestion.
- Filegate does not terminate TLS and has no REST request limiter. Enforce TLS,
  exposure policy, and REST limits at a reverse proxy or private network.
- Activity is not a durable audit log, and OpenTelemetry tracing is not
  implemented.
- S3 is path-style and does not expose object versioning. Filegate's internal
  version history is administered through REST.
- The published Filegate container is Linux AMD64 only; packages cover Linux
  AMD64 and ARM64. The Admin app currently has no published release artifact.

For backup and restore procedures, filesystem repair, detector recovery, and
operational checklists, see the [system administration reference](/docs/en/reference/sysadmin).
