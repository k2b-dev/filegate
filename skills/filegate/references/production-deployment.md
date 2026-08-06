# Production Deployment

Use this reference to decide whether Filegate fits a production workload and to
produce a deployment, backup, restore, or upgrade procedure.

## Suitability boundaries

Filegate is a Linux-only, single-node, single-tenant storage gateway.

- Run exactly one active daemon for a runtime config store, metadata index, and
  writable mount set. There is no replication, leader election, or shared
  Pebble mode.
- One REST bearer token grants full file and configuration authority. S3 keys
  may be restricted to buckets and rate-limited.
- Individual operations publish atomically. There is no transaction across
  multiple files or requests and no cross-request point-in-time snapshot.
- HTTP and S3 writes update the index immediately. Raw filesystem changes are
  eventual through detection and reconciliation and do not create automatic
  versions.
- Do not nest btrfs subvolumes inside a watched btrfs root. External deletion
  of a nested subvolume can stall the parent detector until daemon restart.
- Filegate listeners are cleartext and REST has no built-in rate limiter.
  Terminate TLS and enforce exposure/rate policy at a proxy or private network.
- Activity is an in-memory ring, not a durable audit log. OpenTelemetry tracing
  is not implemented.
- S3 is path-style only and does not expose S3 object versioning.

If the workload requires active-active writes, tenant isolation, per-path REST
authorization, durable audit, or multi-object transactions, do not imply that
Filegate supplies those properties. Put the missing boundary in the owning
application/infrastructure or choose a different storage service.

## Recommended deployment

Prefer the Linux package and systemd unit. Packages are published for Linux
AMD64 and ARM64. The published Filegate container is currently Linux AMD64 only
and runs as UID/GID `65532`; bind mounts must be writable by that identity and
preserve user xattrs.

The Admin app is built and deployed separately from source; no Admin release
package or container is currently published. It is a full-authority operator
surface, not a read-only dashboard. With multiple Admin replicas, share
`ADMIN_SESSION_SECRET` and configure `REDIS_URL` for shared login rate limits.

## Durable state

| State | Authority | Recovery |
|---|---|---|
| Configured file roots plus `user.filegate.id` xattrs | Bytes, paths, stable IDs, ownership, versions | Required backup; preserve xattrs. |
| `storage.runtime_config_path` | Applied manifest, generated REST token, S3 credentials | Required backup; not rebuildable. |
| `storage.index_path` | Listings, lookup, upload-session metadata | Optional same-point backup or offline rebuild. |
| Bootstrap config and external secrets | Early-start inputs | Back up in their owning deployment system. |

Do not copy a live Pebble directory independently from changing data. Stop the
daemon or take coordinated filesystem snapshots. File-copy tools must preserve
xattrs; `rsync -aHAX --numeric-ids` is an example.

## Restore and verify

1. Stop every instance.
2. Restore data roots and xattrs.
3. Restore the runtime config store and bootstrap/secret inputs.
4. Restore the index only from the same backup point; otherwise run
   `fg index rescan --new --config <path>` while stopped.
5. Start exactly one daemon.
6. Check unauthenticated `/health`, authenticated `/v1/health`, and
   `/v1/system/info`; resolve a known stable ID and perform a controlled
   read/write.

An index rebuild loses in-progress upload sessions. The activity ring is never
restored.

## Upgrade and rollback

Package upgrades are offline and refuse to replace files while
`filegate.service` runs. Before upgrading, make a verified backup and record the
binary/image version and manifest revision. After starting, run the verification
above.

For rollback, reinstall the previous artifact. Restore data, runtime store, and
matching index together only if durable state must be rolled back. There is no
general on-disk schema downgrade promise, so rehearse forward and rollback paths
on a copy of production state.

## Filesystem selection

ext4, XFS, and btrfs are supported when roots are writable and support user
xattrs. btrfs change detection is an optimization; polling is the portable
fallback. Versioning probes real `FICLONE` support:

- `auto`: enable only if every configured mount supports reflinks.
- `on`: enable everywhere, using reflink, mixed, or byte-copy behavior.
- `off`: disable versioning.

Read `/v1/system/info` for effective detector/versioning modes and mount
capabilities. Use `/v1/health` for readiness; `/health` is liveness only and
`/v1/stats` is not readiness.

After any external nested-subvolume deletion, restart Filegate and confirm
detector cycles advance in `/v1/system/runtime` before trusting external-change
ingestion.
