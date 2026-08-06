# Sysadmin Guide

This guide is for deploying, operating, and maintaining Filegate in production.

## 1. Deployment Modes

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

## 3. Required Settings

Minimum required for REST or mixed REST+S3 deployments:

```yaml
auth:
  bearer_token: "REPLACE_ME"
storage:
  base_paths:
    - /var/lib/filegate/data
```

For S3-only deployments, `auth.bearer_token` may be empty when
`s3.enabled: true`; the REST `/v1` API stays locked down with 401,
while `/health` and optional `/metrics` remain on `server.listen`.

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

The package intentionally does not auto-start the service.

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

## 6. Filesystem Choice (ext4 vs btrfs)

Both ext4 and btrfs are supported.

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

### Practical impact for Filegate detectors

- On btrfs, Filegate can process foreign filesystem activity (NFS writers, local tools, sidecar processes) much more efficiently by reading transid/generation deltas.
- On ext4/xfs, Filegate cannot use `find-new`; detector fallback is polling-based and must repeatedly scan/check directory trees and files.
- As tree size and churn increase, ext4/xfs polling overhead can grow sharply compared to btrfs delta-based ingestion.

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
  `poll`; the System page reports this reason instead of hiding the fallback.

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

## 10. Routine Operations

Health and stats:

```bash
curl -fsS http://127.0.0.1:8080/health
curl -fsS -H 'Authorization: Bearer <token>' http://127.0.0.1:8080/v1/stats
```

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
- The command is intentionally offline-only and exits with an error if index files are in use.
- By default it creates a timestamped backup of the previous index directory.
- Use `--skip-backup` only when you explicitly do not want a rollback artifact.

Recommended cadence:

- check health and error logs continuously
- review stats trends daily (index size, cache usage, disk usage)
- run `index rescan --new` for severe index corruption scenarios (daemon stopped)

## 11. Upgrade and Rollback

Before upgrade:

- backup config
- snapshot or backup data roots and index path
- validate package signature/source in your normal process

Upgrade:

- stop `filegate` before installing a `.deb`/`.rpm` upgrade
- install the new package or container image
- start service
- verify `/health`, `/v1/stats`, and key read/write flows

Package upgrades fail before replacing files when `filegate.service` is still active. The package script prints the stop instruction and leaves the existing install in place.

Rollback:

- restore previous package/image
- restore index/data snapshot if required
- restart and validate critical paths

## 12. Troubleshooting Quick Table

- `401 unauthorized`: wrong/missing bearer token.
- `permission denied` on write: systemd `ReadWritePaths` mismatch.
- high metadata latency: cache too small, storage pressure, or detector churn.
- delayed external updates: detector backend/poll settings too conservative.
- chunk finalize failures: checksum mismatch or incomplete upload set.
- `507 insufficient storage`: upload guard (`upload.min_free_bytes`) prevented writes near full disk.
- index startup error with malformed/corrupt Pebble files: stop daemon, run `fg index rescan --new`, start daemon.
