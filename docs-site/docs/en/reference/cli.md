---
title: CLI reference
navTitle: CLI
section: Reference
order: 210
description: Complete Filegate CLI command reference.
tags: [reference, cli]
---

# CLI reference

This reference catalogs Filegate CLI commands for operators using `filegate` or the short `fg` command installed by packages.

## Commands

| Command | Scope | Meaning |
|---|---:|---|
| `fg serve` | Service process | Start the Filegate REST listener and optional S3 listener. |
| `fg config show` | Config file | Print the resolved config as YAML or JSON. |
| `fg config schema` | Local CLI | List every config key with type, scope and default. Needs no config file. |
| `fg config validate` | Config file | Validate resolved config. |
| `fg config set` | Config file | Set one or more config values offline. |
| `fg config plan -f <manifest>` | Running service | Validate and diff a complete desired-state manifest. |
| `fg config apply -f <manifest>` | Running service | Apply a complete manifest with revision conflict protection. |
| `fg config mount add` | Config file | Add a storage mount. |
| `fg config mount remove` | Config file | Remove a storage mount. |
| `fg config s3 key generate` | Local CLI | Generate an S3 access key and secret. |
| `fg config s3 key list` | Config file | List configured S3 keys. |
| `fg config s3 key add` | Config file | Add an S3 key entry. |
| `fg config s3 key disable <access-key>` | Config file | Clear a key's bucket allowlist. |
| `fg config s3 key remove <access-key>` | Config file | Remove a key entry. |
| `fg index rescan` | Index path and mounts | Rebuild in-memory/index state by scanning mounts. |
| `fg index stats` | Index path | Print index entry count. |
| `fg health` | Service listener | Call `/health`. |
| `fg status` | Service listener | Call `/v1/stats` and print JSON. |

## Global and common flags

| Flag | Type | Scope | Meaning |
|---|---|---:|---|
| `--config` | string | Config commands, serve, index, health/status resolution | Config file path. |
| `--host` | string | `health`, `status`, config `plan/apply` | API base URL override. |
| `--token` | string | `status`, config `plan/apply` | Bearer token override. |
| `--timeout` | duration | `health`, `status`, config `plan/apply` | HTTP request timeout. |

Config `plan/apply` also accept `FILEGATE_HOST` and `FILEGATE_TOKEN`. Without explicit values they resolve the local listener and bearer token from the bootstrap config.

## `fg config show`

| Flag | Type | Default | Meaning |
|---|---|---:|---|
| `--format` | enum | `yaml` | Output format: `yaml` or `json`. |
| `--show-secrets` | boolean | `false` | Print secret values instead of redacting them. |

## `fg config schema`

| Flag | Type | Default | Meaning |
|---|---|---:|---|
| `--format` | enum | `table` | Output format: `table`, `json` or `markdown`. |

`markdown` renders the published [Config reference](/docs/en/reference/config); `make docs-config` regenerates it.

## `fg config set`

`fg config set` edits the offline bootstrap file. It does not mutate a running daemon. Manifest-owned deployment state should be changed in the manifest and applied instead.

| Flag | Type | Default | Meaning |
|---|---|---:|---|
| `--no-backup` | boolean | `false` | Skip timestamped config backup before replacing the config file. |

## `fg config plan` and `fg config apply`

Both commands require a version 1 manifest through `--file` / `-f` and use the authenticated HTTP API.

| Flag | Type | Default | Meaning |
|---|---|---:|---|
| `--file`, `-f` | string | required | Versioned manifest YAML. |
| `--host` | string | resolved | Filegate API base URL. |
| `--token` | string | resolved | Bearer token. Mutually exclusive with `--token-file`. |
| `--token-file` | string | empty | File containing the bearer token. |
| `--actor` | string | `filegate-cli` | Operator label persisted with the apply. |
| `--format` | enum | `text` | `text` or `json`. |
| `--timeout` | duration | `15s` | Per-request timeout. |

`apply` plans first and sends the observed current revision to the apply route. A concurrent apply returns a conflict instead of overwriting newer desired state.

## Host and token resolution

For `health`, `status`, and config `plan/apply`, host resolution uses `--host`,
then `FILEGATE_HOST`, then `server.listen` from the selected bootstrap config.
Listener values such as `:8080`, `0.0.0.0:8080`, and `[::]:8080` are normalized
to localhost for local CLI calls.

Authenticated commands resolve their token from `--token`, `--token-file`
where supported, `FILEGATE_TOKEN`, then an explicit bootstrap
`auth.bearer_token`. A generated token exists only in the runtime store and is
not read back by the CLI; pass it explicitly for local or remote automation.

## S3 key commands

| Command | Flag | Type | Meaning |
|---|---|---|---|
| `fg config s3 key add` | `--access-key` | string | Access key to add; generated when omitted. |
| `fg config s3 key add` | `--secret-key` | string | Secret key to add; generated when omitted. |
| `fg config s3 key add` | `--bucket` | string array | Allowed bucket name. Repeat for multiple buckets. |
| `fg config s3 key add` | `--all-buckets` | boolean | Grant access to every configured mount. |
| `fg config s3 key add` | `--requests-per-second` | integer | Sustained request rate. `0` disables throttling. |
| `fg config s3 key add` | `--burst` | integer | Burst size. Defaults to RPS when unset. |
| `fg config s3 key add` | `--no-backup` | boolean | Skip timestamped backup before replacing config. |
| `fg config s3 key list` | `--show-secrets` | boolean | Print S3 secrets instead of redacting them. |

## Index commands

| Command | Flag | Type | Meaning |
|---|---|---|---|
| `fg index rescan` | `--new` | boolean | Recreate index directory before rescanning. Daemon must be stopped. |
| `fg index rescan` | `--skip-backup` | boolean | With `--new`, do not create an index backup. |

## Serve config flags

`fg serve` accepts the same config-value flags as `fg config set`. Serve flags override the resolved config only for that process.
