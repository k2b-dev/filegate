# S3 Listener Configuration

The S3-compatible HTTP listener is **off by default**. This page covers turning it on, the auth model, the mount→bucket mapping, and the TLS-termination requirement.

## Minimal config

```yaml
s3:
  enabled: true
  listen: ":9100"
  region: "us-east-1"
  access_key: "AKIAIOSFODNN7EXAMPLE"
  secret_key: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
```

This single-key form grants the configured key access to **every** mount. It is a bootstrap seed: Filegate imports it into the runtime store on first start, then the runtime store owns the key. Deployments that need separate credentials should seed the `keys` list below or create keys through the S3 resource API after bootstrap.

When `s3.enabled=true`, filegate validates every mount name as an S3 bucket name on startup. Invalid mount names — not lowercase, > 63 chars, IP-format, AWS-reserved prefix/suffix — fail startup loudly so you catch the misconfiguration before clients hit it.

The S3 listener binds on its own port (`s3.listen`), separate from the REST listener (`server.listen`). Bind it to an internal interface in production so only your reverse proxy can reach it.

---

## Multi-key bootstrap seed

For deployments with multiple users / clients, replace the single-tenant fields with a `keys` list:

```yaml
s3:
  enabled: true
  listen: ":9100"
  region: "us-east-1"
  keys:
    - access_key: "AKIAALICEKEY00000001"
      secret_key: "alice-secret-keep-this-private-please"
      buckets: ["alice-photos", "alice-docs"]

    - access_key: "AKIABOBKEY0000000001"
      secret_key: "bob-secret-keep-this-private-please"
      buckets: ["bob-archive"]

    - access_key: "AKIABACKUPKEY0000001"
      secret_key: "backup-tool-secret-restic-or-similar"
      buckets: ["backups"]

    - access_key: "AKIAOPSKEY00000000001"
      secret_key: "ops-admin-secret-private-please"
      buckets: ["*"]    # wildcard — sees every configured mount
```

### Whitelist semantics

- `buckets: ["a", "b"]` — the key may operate on mounts `a` and `b`. ListBuckets returns exactly those two. Any access to other mounts returns `403 AccessDenied` (the bucket's existence is **not** revealed).
- `buckets: ["*"]` — the key may operate on every configured mount. ListBuckets returns the full mount list.
- `buckets: []` (empty list) — the key authenticates but every bucket-scoped op returns 403. Useful for staging revocation without removing the entry.

### Validation at startup

Filegate refuses to start when:

- `s3.enabled=true` but no credentials (neither legacy `access_key`/`secret_key` nor any `keys` entry) are configured.
- A `keys` entry has an empty `access_key` or `secret_key`.
- Two `keys` entries share the same `access_key` (paste-twice typo — silent override would be a security hazard).
- A `keys` entry's `buckets` list references a mount name that doesn't exist (catches typos like `"buckte"` instead of `"bucket"`).

These errors are loud and fail-fast — operators catch the misconfiguration up-front instead of debugging mysterious 403s later.

### Combining single-key and multi-key seeds

The single-key `access_key`/`secret_key` fields and the `keys` list **coexist**. Filegate imports both into an empty runtime store on first start. The single key is treated as a `"*"` wildcard entry. A duplicate access key between the single-key fields and a `keys` entry is rejected at startup.

After that first seed, changes to these fields do not recreate, update, or delete runtime keys. This prevents a key removed through the admin or API from returning after a restart.

### Managing keys after bootstrap

Create, rotate, disable, and delete keys from the admin S3 page or the `/v1/s3/keys` routes. Changes apply immediately and persist in `storage.runtime_config_path`. A new or rotated secret is returned once and cannot be retrieved later.

The `fg config s3 key` commands edit the offline bootstrap YAML. They are useful for preparing a new deployment, but they do not modify the runtime store of an existing deployment. Restarting does not re-import the file after the store has been seeded.

To prepare a seed credential, generate a pair:

```bash
fg config s3 key generate
```

Add it to the bootstrap YAML:

```bash
fg config s3 key add --config /etc/filegate/conf.yaml \
  --bucket alice-photos \
  --access-key FGALICENEW \
  --secret-key '<new-secret>'
```

Omit `--access-key` and/or `--secret-key` to let the CLI generate the missing values. Use `--all-buckets` instead of `--bucket` for an admin key.

You can also disable or remove entries from a bootstrap file before its first use:

```bash
fg config s3 key disable --config /etc/filegate/conf.yaml FGALICEOLD
```

The disabled seed has an empty bucket list, so every bucket operation returns `403 AccessDenied`. Remove it from the bootstrap file with:

```bash
fg config s3 key remove --config /etc/filegate/conf.yaml FGALICEOLD
```

Every mutating `fg config` command validates the resulting YAML and creates a timestamped backup by default. It does not change live runtime resources.

---

## Mount → bucket mapping

Each entry in `storage.base_paths` becomes one bucket; the bucket name is the basename of the path:

```yaml
storage:
  base_paths:
    - /var/lib/filegate/photos     # → bucket "photos"
    - /var/lib/filegate/backups    # → bucket "backups"
    - /var/lib/filegate/archive    # → bucket "archive"
```

The path leaf is the bucket name. To rename a bucket, rename the directory and restart filegate (the index will rebuild references; existing object IDs are preserved).

### Bucket-name rules (S3 spec)

When `s3.enabled=true`, every mount basename must:

- Be 3-63 characters.
- Use only lowercase letters, digits, and hyphens.
- Not start or end with a hyphen.
- Not contain dots; Filegate deliberately keeps bucket names path-style-safe.
- Not match an IP-address shape (`192.168.1.1`).
- Not start with `xn-`, `sthree-`, or `amzn-s3-demo-` (AWS reservations).
- Not end with `-s3alias`, `--ol-s3`, `--x-s3`, or `--table-s3`.
- Not be `.fg-versions` or `.fg-uploads` (filegate-reserved internal namespaces).

Filegate fails startup with a clear error when any mount fails this check.

---

## TLS termination

The S3 listener speaks **plain HTTP**. Production deployments must put a reverse proxy (Traefik, Caddy, nginx) in front for TLS termination.

This is intentional: SigV4 already authenticates and integrity-protects the request body, so plain-HTTP between a trusted reverse proxy and filegate is safe **inside the same trusted network**. Adding TLS inside the daemon would duplicate work the proxy is already doing better.

### Traefik example

```yaml
# traefik labels on the filegate service
- "traefik.enable=true"
- "traefik.http.routers.filegate-s3.rule=Host(`s3.example.com`)"
- "traefik.http.routers.filegate-s3.entrypoints=websecure"
- "traefik.http.routers.filegate-s3.tls.certresolver=letsencrypt"
- "traefik.http.services.filegate-s3.loadbalancer.server.port=9100"
```

### Caddy example

```caddy
s3.example.com {
  reverse_proxy filegate:9100
}
```

### Why path-style hosts work

S3 path-style addressing means the bucket name is in the URL path (`https://s3.example.com/{bucket}/{key}`), not the hostname. A single hostname routes all bucket traffic — no per-bucket DNS records, no wildcard certs.

---

## Region

Set `s3.region` to whatever string you want clients to sign with. Filegate doesn't care about the value; it only matters that the client's signature scope matches.

```yaml
s3:
  region: "us-east-1"     # default — works with everything
```

Operators with one filegate instance can keep `us-east-1`. Operators running multiple filegate instances behind one client may set distinct region names so misrouted requests fail fast at signature verification.

---

## Access logs

The same `server.access_log_enabled` flag controls both the REST and S3 access logs. Per-request lines look like:

```
[filegate-s3] PutObject bucket=alice-photos key=2024/cat.jpg by=AKIAALICEKEY00000001 create
[filegate-s3] CompleteMultipartUpload bucket=backups key=archive.tar by=AKIABACKUPKEY0000001 uploadId=… parts=42 etag=… replayed=false
[filegate-s3] recover: committing upload … bucket=backups key=archive.tar has no durable record; leaving for CompleteMultipartUpload retry (stage=/srv/filegate/backups/.fg-uploads/s3-…)
```

The `by=` field is the verified access key. No secrets ever appear in the log.
The recovery line means startup found an incomplete multipart Complete after a crash or forced shutdown. Retry CompleteMultipartUpload with the original parts list when the client still has it. If the upload is abandoned, confirm the object was not created, then abort the upload or remove the logged staging directory.

---

## Write concurrency

Large folder uploads can open many multipart uploads at once. `s3.max_concurrent_writes` bounds the number of S3 `PutObject`, `UploadPart`, and copy writes that may write to local storage concurrently.

```yaml
s3:
  max_concurrent_writes: 32  # 0 uses the default
```

Lower the value for small disks, container tmpfs mounts, or low `ulimit -n` settings. The limit does not reject clients; extra write requests wait for a slot.

---

## Per-mount internal layout

The S3 listener uses two internal namespaces under each mount root:

- `<mount>/.fg-versions/<file-id>/<version-id>.bin` — the REST versioning feature's per-file blobs (reflinked when supported, byte-copied otherwise).
- `<mount>/.fg-uploads/s3-<uploadId>/` — multipart upload staging (`parts/00001.bin`, …, and ephemeral `complete.tmp`). Active multipart metadata and part rows are stored in Pebble.

Both are filegate-private. Object keys can't reach them: the validator rejects `.fg-versions`, `.fg-uploads`, and Filegate's internal `.filegate-tmp-*` atomic-write names as path segments.

---

## See also

- [s3-api.md](./s3-api.md) — supported operations + deviations.
- [s3-clients.md](./s3-clients.md) — config snippets for clients.
