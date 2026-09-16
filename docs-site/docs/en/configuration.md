---
title: Configure roots
section: Operate
order: 10
description: Static YAML settings and independent index and version policies.
---

# Configure roots

Filegate reads a YAML configuration file at startup. `--config` selects the
file; the default is `/etc/filegate/conf.yaml`. Use a single YAML document
with the fields listed below. Unknown fields fail validation. Restart the
daemon to apply changes.

```yaml
server:
  listen: "127.0.0.1:8080"
  public_url: "https://files.example.org"
  allowed_origins: ["https://app.example.org"]
auth:
  token_file: /etc/filegate/token
state_dir: /var/lib/filegate
uploads:
  max_file_size: 10GiB
roots:
  - name: documents
    path: /srv/filegate/documents
    index: true
    versioning:
      enabled: true
      cooldown: 1m
      keep:
        last: 10
        daily: 30
        monthly: 12
  - name: shared
    path: /mnt/shared
    index: false
```

| Setting | Behavior |
| --- | --- |
| `server.listen` | Defaults to `127.0.0.1:8080`. |
| `server.public_url` | Required HTTP(S) origin for direct URLs and administrative CLI calls. |
| `server.allowed_origins` | Explicit browser origins; empty disables cross-origin access. |
| `auth.token_file` | Absolute path to a token of 32–4096 bytes, with optional surrounding whitespace. |
| `state_dir` | Required absolute directory outside every root; one active daemon owns it. |
| `uploads.max_file_size` | Defaults to `10GiB`; integer bytes or `B`, `KiB`, `MiB`, `GiB`. Applies to every upload method. |
| `roots[].name` | Unique name: letters, digits, `_` and `-`; first character alphanumeric; at most 64 characters. |
| `roots[].path` | Existing absolute directory. Canonical root paths cannot overlap each other or state. |
| `roots[].index` | Defaults to false. Enable metadata search and stable xattr IDs. |
| `roots[].versioning.enabled` | Defaults to false. Requires index enabled. |
| `roots[].versioning.cooldown` | Defaults to `1m`; zero captures each successful overwrite. |
| `roots[].versioning.keep` | Defaults to `{last: 10, daily: 30, monthly: 12}` when the entire `keep` section is absent; see [versioning](versioning.md). |

Each root has independent maintenance and history. A rebuild pauses publication
and other root mutations while it walks that root. Upload bodies can still be
staged, and other roots remain available. External writers are not locked: pause
them separately if you need a consistent scan.

Root names and paths bind durable state. Do not repoint an existing root name at
unrelated storage. Filegate rejects a changed binding. Do not run another daemon
against the same writable roots, even if it uses a different state directory.
