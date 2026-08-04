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

Use the [Admin UI](admin) for browser-based file operations, runtime metrics, activity inspection, and index rescans.

## Service lifecycle

| Task | Scope | Command |
|---|---:|---|
| Start service | Host service | `sudo systemctl start filegate` |
| Stop service | Host service | `sudo systemctl stop filegate` |
| Enable on boot | Host service | `sudo systemctl enable filegate` |
| Read status | Host service | `sudo systemctl status filegate` |
| Reload unit files | Host systemd | `sudo systemctl daemon-reload` |

Filegate does not hot-reload the YAML config. Restart the service after changing config.

## Health and status

| Signal | Scope | Command | Meaning |
|---|---:|---|---|
| HTTP health | Service listener | `curl -fsS http://127.0.0.1:8080/health` | Process accepts HTTP requests. |
| CLI health | Configured service | `fg health --host http://127.0.0.1:8080` | Same health check through CLI. |
| Local status | Config file and service | `fg status --config /etc/filegate/conf.yaml` | Reads local config and runtime health. |

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
