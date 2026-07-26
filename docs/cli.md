# CLI Reference

Filegate CLI is intentionally local-ops focused. The installed binary is `filegate`; packages also install `fg` as the short command used in these examples.

## Core Commands

```bash
fg serve [--config /etc/filegate/conf.yaml]
fg config show [--config /etc/filegate/conf.yaml] [--format yaml|json] [--show-secrets]
fg config schema [--format table|json|markdown]
fg config validate [--config /etc/filegate/conf.yaml]
fg config set --config /etc/filegate/conf.yaml [config flags...]
fg config s3 key generate
fg config s3 key list [--config /etc/filegate/conf.yaml] [--show-secrets]
fg config s3 key add --config /etc/filegate/conf.yaml (--bucket <name>... | --all-buckets) [--access-key <key>] [--secret-key <secret>]
fg config s3 key disable --config /etc/filegate/conf.yaml <access-key>
fg config s3 key remove --config /etc/filegate/conf.yaml <access-key>
fg config mount list [--config /etc/filegate/conf.yaml]
fg config mount add --config /etc/filegate/conf.yaml <path>
fg config mount remove --config /etc/filegate/conf.yaml <path-or-bucket>
fg health [--host <host-or-url>] [--config /etc/filegate/conf.yaml] [--timeout 10s]
fg status [--host <host-or-url>] [--token <bearer>] [--config /etc/filegate/conf.yaml] [--timeout 10s]
fg index stats [--config /etc/filegate/conf.yaml]
fg index rescan [--config /etc/filegate/conf.yaml]
fg index rescan --new [--skip-backup] [--config /etc/filegate/conf.yaml]
```

## Semantics

- `serve`:
  - Starts the HTTP server and detector.
  - Linux-only.
  - Accepts every config value flag listed below as a one-shot runtime override.
- `config show`:
  - Prints the resolved config after defaults and environment overrides.
  - Redacts bearer tokens, S3 secrets, and metrics tokens unless `--show-secrets` is set.
- `config schema`:
  - Lists every config key with its type, scope, default and usage.
  - Needs no config file; the catalog comes from the binary.
  - `--format markdown` renders the published config reference. `make docs-config` writes it, and a test fails when the committed page drifts.
- `config validate`:
  - Loads the config with the same resolver as `serve`.
  - Exits non-zero on invalid values, missing required mounts, invalid S3 key references, or duplicate S3 access keys.
- `config set`:
  - Writes config values to YAML.
  - Requires explicit `--config`; `FILEGATE_CONFIG` is not enough for mutating commands.
  - Writes a timestamped backup by default, validates the temporary result, then atomically replaces the config.
  - Does not change a running daemon; restart filegate to apply the edited file.
- `config s3 key generate`:
  - Prints a new random access key and secret.
- `config s3 key add`:
  - Adds a multi-tenant `s3.keys` entry.
  - Generates either credential when `--access-key` or `--secret-key` is omitted.
  - Requires `--bucket` at least once or `--all-buckets`.
  - Validates bucket names against configured mounts.
- `config s3 key disable`:
  - Clears the key's bucket list. The key still authenticates but has no bucket access.
- `config s3 key remove`:
  - Removes a key entry. If the access key is the legacy `s3.access_key`, it clears the legacy key pair.
- `config mount add`:
  - Adds a storage mount path after checking that it exists and is a directory.
  - On Linux, also runs the same write/xattr health probe used at daemon startup.
  - When S3 is enabled, validates the mount basename as an S3 bucket name.
- `config mount remove`:
  - Removes a path from `storage.base_paths` by exact path or bucket basename.
  - Does not delete data from disk.
- `health`:
  - Calls `GET /health`.
  - No token required.
- `status`:
  - Calls `GET /v1/stats`.
  - Requires bearer token.
- `index stats`:
  - Reads local Pebble index and prints entity count.
- `index rescan`:
  - Rebuilds index state from filesystem by rescanning roots.
- `index rescan --new`:
  - Recreates index directory first, then rescans.
  - Expects daemon to be stopped.
  - Creates timestamped backup by default.
- `index rescan --new --skip-backup`:
  - Same as above, but without backup creation.

## Host/Token Resolution

For `health` and `status`:

- `--host` and `--token` are optional.
- If omitted, values are read from config.
- Config load order:
  1. `--config`
  2. `FILEGATE_CONFIG`
  3. default config candidates used by the daemon

Special host handling:

- `server.listen` values like `:8080`, `0.0.0.0:8080`, or `[::]:8080` are normalized to localhost for local CLI checks.

## Config Value Flags

`fg serve` accepts these flags as runtime overrides. `fg config set --config <path>` accepts the same flags and writes them to YAML.

```bash
--server-listen
--server-public-url
--server-trusted-proxies
--server-cors-allowed-origins
--server-cors-allowed-methods
--server-cors-allowed-headers
--server-cors-exposed-headers
--server-cors-max-age
--server-cors-allow-credentials
--server-write-timeout
--server-access-log-enabled
--server-shutdown-timeout
--auth-bearer-token
--storage-base-paths
--storage-index-path
--detection-backend
--detection-poll-interval
--cache-path-cache-size
--jobs-workers
--jobs-queue-size
--jobs-thumbnail-workers
--jobs-thumbnail-queue-size
--upload-expiry
--upload-cleanup-interval
--upload-max-chunk-bytes
--upload-max-upload-bytes
--upload-max-session-upload-bytes
--upload-max-concurrent-segment-writes
--upload-min-free-bytes
--thumbnail-lru-cache-size
--thumbnail-max-source-bytes
--thumbnail-max-pixels
--versioning-enabled
--versioning-cooldown
--versioning-min-size-for-auto-v1
--versioning-retention-bucket
--versioning-pruner-interval
--versioning-max-pinned-per-file
--versioning-pinned-grace-after-delete
--versioning-max-label-bytes
--s3-enabled
--s3-listen
--s3-region
--s3-access-key
--s3-secret-key
--s3-max-concurrent-writes
--s3-key
--s3-cleanup-done-retention
--s3-cleanup-aborted-retention
--s3-cleanup-stuck-upload-max-age
--s3-cleanup-interval
--metrics-enabled
--metrics-path
--metrics-token
--activity-ring-buffer-size
```

List fields use repeatable flags:

```bash
fg config set --config /etc/filegate/conf.yaml \
  --storage-base-paths /srv/filegate/photos \
  --storage-base-paths /srv/filegate/backups
```

Composite list fields use `key=value` pairs:

```bash
fg config set --config /etc/filegate/conf.yaml \
  --versioning-retention-bucket keep_for=1h,max_count=-1 \
  --versioning-retention-bucket keep_for=24h,max_count=24

fg config set --config /etc/filegate/conf.yaml \
  --s3-key 'access_key=FGALICE,secret_key=alice-secret,buckets=photos|docs,requests_per_second=20,burst=40'
```
