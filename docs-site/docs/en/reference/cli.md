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
| `--host` | string | `health`, `status` | API base URL override. |
| `--token` | string | `status` | Bearer token override. |
| `--timeout` | duration | `health`, `status` | HTTP request timeout. Default `10s`. |

## `fg config show`

| Flag | Type | Default | Meaning |
|---|---|---:|---|
| `--format` | enum | `yaml` | Output format: `yaml` or `json`. |
| `--show-secrets` | boolean | `false` | Print secret values instead of redacting them. |

## `fg config schema`

| Flag | Type | Default | Meaning |
|---|---|---:|---|
| `--format` | enum | `table` | Output format: `table`, `json` or `markdown`. |

`markdown` renders the published [Config reference](config); `make docs-config` regenerates it.

## `fg config set`

`fg config set` accepts every config flag listed in [Config reference](config). Mutating config commands require explicit `--config`.

| Flag | Type | Default | Meaning |
|---|---|---:|---|
| `--no-backup` | boolean | `false` | Skip timestamped config backup before replacing the config file. |

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
