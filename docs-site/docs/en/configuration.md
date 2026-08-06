---
title: Configuration
navTitle: Configuration
section: Use Filegate
order: 40
description: Manage Filegate as versioned desired state with manifest plan and apply.
tags: [configuration, yaml, cli, gitops]
---

# Configuration

Use a manifest for service behavior that belongs in source control. Filegate validates and stores the complete last-applied desired state in its runtime config store. The Settings page shows that state but does not edit it.

Bootstrap settings and credentials stay outside the manifest. This keeps recovery possible when the runtime store is unavailable and keeps secrets out of the desired-state document.

## Write a manifest

A manifest has one versioned envelope and a nested `config` mapping:

```yaml
version: 1
config:
  server:
    public_url: https://files.example.com
    access_log_enabled: true
    cors:
      allowed_origins:
        - https://app.example.com
  upload:
    max_upload_bytes: 1073741824
  versioning:
    enabled: "on"
    pruner_interval: 10m
```

Commit this file to the repository that owns the deployment. The document is a complete replacement, not a patch:

- A key present in the new manifest is manifest-managed.
- A key removed from the new manifest is removed from managed state and falls back to environment, bootstrap file, or its built-in default.
- `null` is rejected. Omit a key to stop managing it.
- Unknown, secret, bootstrap-owned, and resource-owned keys are rejected.

## Plan the change

Plan against the same authenticated API locally or remotely:

```sh
fg config plan -f filegate.manifest.yaml \
  --host https://files.example.com \
  --token-file /run/secrets/filegate-token
```

The plan reports additions, changes, removals, their activation class, the current revision, and the proposed revision. It does not write the runtime store or change the running process.

For a local daemon, `--host` can be omitted because the bootstrap config
contains `server.listen`. Token flags can be omitted only when the bootstrap
config contains an explicit `auth.bearer_token`. A generated token exists only
in the runtime store and is deliberately not read back by the CLI; pass it via
`--token-file` or `FILEGATE_TOKEN`.

## Apply the manifest

```sh
fg config apply -f filegate.manifest.yaml \
  --host https://files.example.com \
  --token-file /run/secrets/filegate-token
```

`apply` obtains a fresh plan and sends its current revision as a precondition. Filegate returns `409 Conflict` if another actor applied a manifest in between. Run the command again to inspect the new state instead of overwriting it blindly.

An identical revision is a no-op.

## Local and remote authentication

`plan` and `apply` always use the bearer-authenticated HTTP API. They never open Pebble directly.

Host resolution, highest precedence first:

1. `--host`
2. `FILEGATE_HOST`
3. `server.listen` in the bootstrap config

Token resolution, highest precedence first:

1. `--token`
2. `--token-file`
3. `FILEGATE_TOKEN`
4. `auth.bearer_token` in the bootstrap config

The bootstrap config is selected by `--config`, then `FILEGATE_CONFIG`, then the normal default candidates. `--token` and `--token-file` are mutually exclusive.

The same bearer token grants file and configuration authority. Keep it in a secret store and use a protected deployment runner for remote apply.

## Runtime and static activation

Every manifest-owned key has one activation class:

| Class | Apply behavior |
|---|---|
| Runtime | The validated value is published to the live configuration snapshot immediately. |
| Static | The desired value is stored, while the process keeps its effective startup value until restart. |

`GET /v1/config` and the Settings page report effective and desired values separately. A static difference appears in `restartRequired`; after a successful restart, effective and desired match.

See [Config reference](/docs/en/reference/config) for each key's activation and owner.

## Bootstrap and resource boundaries

The following values do not belong in a manifest:

- `storage.runtime_config_path`, because Filegate must know it before it can open Pebble and read the manifest.
- Secret settings such as `auth.bearer_token` and `metrics.token`.
- S3 credentials and access-key lists, which are managed as runtime resources.

Bootstrap values come from defaults, the bootstrap YAML file, environment, or `fg serve` flags. `fg config show`, `validate`, and `set` operate on this offline bootstrap configuration; they do not update a running daemon.

S3 keys may be seeded once from bootstrap configuration. After seeding, use the S3 key API or the admin S3 page to create, rotate, disable, and delete them. Deleting a resource does not alter the manifest.

## Stored state

The runtime store at `storage.runtime_config_path` contains:

- The normalized complete manifest.
- Its SHA-256 revision, apply timestamp, and actor.
- Runtime resources such as S3 access keys and a generated REST token.

The store is separate from the rebuildable search index. Back it up with the data; losing it loses the applied desired state and runtime credentials.

## Inspect the running state

The Settings page is read-only for configuration. It shows:

- Manifest revision, apply time, and actor.
- Effective and desired values.
- Provenance from manifest, environment, bootstrap file, or default.
- Static values waiting on a restart.

Runtime resources remain operational on their dedicated admin pages because they have their own lifecycle and are not declarative config keys.

The authenticated API exposes the same state:

| Route | Meaning |
|---|---|
| `GET /v1/config/schema` | Key type, activation, ownership, unit, default, and restart reason. |
| `GET /v1/config` | Manifest metadata plus desired, effective, and provenance for every key. |
| `POST /v1/config/plan` | Validate and diff a complete replacement manifest without changing state. |
| `POST /v1/config/apply` | Apply the complete manifest with an expected-revision precondition. |

## Bootstrap example

Keep only recovery and early-start values here:

```yaml
storage:
  runtime_config_path: /var/lib/filegate/config
  base_paths:
    - /srv/filegate/data
```

Set the bearer token separately through `FILEGATE_AUTH_BEARER_TOKEN` or a secret-backed bootstrap file. Environment variables use `FILEGATE_` plus the uppercase dotted path with underscores.
