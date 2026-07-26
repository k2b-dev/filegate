---
title: Configuration
navTitle: Configuration
section: Use Filegate
order: 40
description: Configure Filegate through YAML, environment variables, the admin UI, and the config API.
tags: [configuration, yaml, cli]
---

# Configuration

This page is for operators configuring Filegate service behavior, storage mounts, upload limits, metrics, activity, versioning, and S3 access.

## Two layers

Filegate splits configuration by when a value is read, not by where it is written.

| Layer | Keys | Read | Changed by |
|---|---:|---|---|
| Static | 11 | Once, during startup | YAML file, environment, `fg serve` flags. Needs a restart. |
| Runtime | 45 | From the live snapshot, per request or per loop iteration | Admin UI, `PATCH /v1/config`, and the same file and environment sources. Takes effect immediately. |

Static keys are the ones a running process cannot honestly change: the listener it already bound, the store directories it already opened, the routes it mounted conditionally. Everything else is runtime.

The config API accepts static keys too, but reports them back as pending a restart rather than pretending they applied. See [Config reference](reference/config) for the scope of every key.

## Start with nothing

Filegate has no required configuration. `fg serve` on a clean machine starts, serves `/var/lib/filegate/data`, and generates an API token that it prints once:

```txt
┌─ filegate: generated an API token ─────────────────────────────────
│
│   <token>
│
│  This is shown only once. Store it now, or set auth.bearer_token
│  to a value of your own and restart.
└────────────────────────────────────────────────────────────────────
```

The recommended path is therefore: set the few static keys you need, start the service, and configure the rest through the admin UI. Only the default base path is created for you — a configured path that does not exist fails the health check loudly instead of being silently created, because a typo there is indistinguishable from data loss.

## Resolution order

Later sources win.

| Source | Scope | Meaning |
|---|---:|---|
| Built-in defaults | Service | Every key has one. |
| Config file | Host filesystem | `--config`, else `FILEGATE_CONFIG`, else the default candidates below. |
| `FILEGATE_*` variables | Process environment | Derived from the config path, for example `FILEGATE_SERVER_LISTEN`. Lists accept comma or semicolon separators. |
| `fg serve` config flags | CLI invocation | One-shot overrides for the current process. |
| Runtime store | Service state | Values written through the admin UI or the config API. Survive restarts. |

Default file candidates, in order: `/etc/filegate/conf.yaml`, `/etc/filegate/conf.yml`, `./conf.yaml`, `./conf.yml`, `/etc/filegate/config.yaml`, `/etc/filegate/config.yml`, `./config.yaml`, `./config.yml`.

Because the runtime store sits on top, a value changed in the admin UI keeps winning over the YAML file until it is cleared. Clearing a key means sending `null` for it, which drops the override and lets the file, environment, or default show through again.

Every value reports where it came from. The admin Settings page shows this as `default`, `file`, `env` or `runtime`, so a setting that ignores your YAML edit explains itself instead of looking broken.

## The runtime store

Runtime values and runtime resources — S3 access keys, the generated API token — live in their own store at `storage.runtime_config_path`, default `/var/lib/filegate/config`.

It is deliberately not the search index. `fg index rescan --new` removes the index directory outright, so anything authoritative sharing it would be destroyed by a routine rebuild. Startup refuses a runtime config path that resolves inside `storage.index_path`.

Back it up with your data. Losing it loses your S3 keys and every setting changed through the UI, not just a cache.

## Seeded resources

S3 access keys can be written in the config file or the environment, but they are seeded, not synchronized: the values are imported the first time the service starts against an empty runtime store, and after that the store owns them. Configured keys are logged and ignored on later starts.

This is why a key deleted in the admin UI stays deleted. A configuration file that keeps recreating credentials an operator revoked is a worse default than one that gets ignored.

To hand ownership back to the file, stop the service and remove the runtime config directory; the next start seeds from the configuration again.

## Edit the file offline

`fg config` edits YAML without touching a running daemon. Mutating commands require `--config`, write a timestamped backup by default, validate the result, and print a restart reminder.

```sh
sudo fg config set --config /etc/filegate/conf.yaml \
  --auth-bearer-token '<strong-token>' \
  --server-public-url 'https://files.example.com'

sudo fg config mount add --config /etc/filegate/conf.yaml /srv/filegate/photos

fg config validate --config /etc/filegate/conf.yaml
```

## Inspect the keys

`fg config schema` lists every key with its type, scope and default, and needs no running server:

```sh
fg config schema
fg config schema --format json
```

## Change a running service

The admin UI Settings page is the intended surface. The same operations are available over HTTP:

| Route | Meaning |
|---|---|
| `GET /v1/config/schema` | Every key with type, scope, unit, default, and why a static key needs a restart. |
| `GET /v1/config` | Effective values and their provenance. Secrets report only whether they are configured. |
| `PATCH /v1/config` | Apply changes. Returns the keys that need a restart. `null` clears an override. |
| `POST /v1/config/validate` | Check a change without applying it. |
| `POST /v1/config/reload` | Re-read the file and environment, for an operator who edited YAML by hand. `SIGHUP` does the same. |

These routes carry the same authority as the bearer token itself — an actor who can PATCH the config can widen CORS or disable access logs. Treat them as an administrative surface, not a read-only one.

## Minimal config

```yaml
server:
  listen: ":8080"

auth:
  bearer_token: "dev-token"

storage:
  base_paths:
    - /srv/filegate/data
  index_path: /var/lib/filegate/index
  runtime_config_path: /var/lib/filegate/config
```

## Complete reference

See [Config reference](reference/config) for every config key, type, default, scope, and matching CLI flag.
