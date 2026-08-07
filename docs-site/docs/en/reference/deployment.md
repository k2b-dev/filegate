---
title: Deployment reference
navTitle: Deployment
section: Deep reference
order: 260
description: Filegate package, systemd, container, HTTP protocol, and Admin deployment details.
tags: [reference, deployment, systemd, docker]
---

# Deployment

## Build Targets

- static Linux binaries via GoReleaser
- `.deb` and `.rpm` via nfpm
- systemd service unit included in packages

Source files:

- GoReleaser config: [`/.goreleaser.yaml`](https://github.com/k2b-dev/filegate/blob/main/.goreleaser.yaml)
- systemd unit: [`packaging/systemd/filegate.service`](https://github.com/k2b-dev/filegate/blob/main/packaging/systemd/filegate.service)
- default config: [`packaging/config/conf.yaml`](https://github.com/k2b-dev/filegate/blob/main/packaging/config/conf.yaml)

## Local Release Build

```bash
goreleaser release --snapshot --clean
```

Artifacts are written to `dist/`.

## Package Install

This is the recommended production deployment path: install the package, configure `/etc/filegate/conf.yaml`, and run Filegate through `filegate.service`.

### Debian/Ubuntu

```bash
sudo dpkg -i ./dist/filegate_<version>_linux_amd64.deb
```

### Rocky/RHEL

```bash
sudo rpm -Uvh ./dist/filegate-<version>-1.x86_64.rpm
```

The package installs:

- binary: `/usr/bin/filegate`
- short command link: `/usr/bin/fg`
- config: `/etc/filegate/conf.yaml`
- systemd unit: `/lib/systemd/system/filegate.service`
- data/log dirs: `/var/lib/filegate`, `/var/log/filegate`

The package installs the service without starting it. Enable and start it after
configuring the storage paths and credentials.

The `fg` command works after package install. To also install the optional shell alias at package install time:

```bash
sudo FILEGATE_INSTALL_ALIAS_FG=1 dpkg -i ./dist/filegate_<version>_linux_amd64.deb
# or:
sudo FILEGATE_INSTALL_ALIAS_FG=1 rpm -Uvh ./dist/filegate-<version>-1.x86_64.rpm
```

The postinstall script writes `alias fg='filegate'` to the invoking user's `.bashrc` or `.zshrc`. For other shells it prints the snippet and leaves shell config unchanged.

## Package Upgrade

Package upgrades are offline operations. The preinstall script refuses to replace package files while `filegate.service` is active, so stop the daemon before upgrading:

```bash
sudo systemctl stop filegate
sudo dpkg -i ./dist/filegate_<version>_linux_amd64.deb
# or:
sudo rpm -Uvh ./dist/filegate-<version>-1.x86_64.rpm
sudo systemctl start filegate
sudo systemctl status filegate
```

If the service is still running, the package manager exits before installing the new version and prints the stop instruction. The script does not stop the service automatically.

## systemd Operations

```bash
sudo systemctl daemon-reload
sudo systemctl enable filegate
sudo systemctl start filegate
sudo systemctl status filegate
```

## HTTP Protocols

The REST listener serves cleartext. It speaks HTTP/1.1 by default, and HTTP/2
over cleartext (h2c) when `server.http2_cleartext` is on. Both then share one
port: the server switches only for a connection that opens with the HTTP/2
preface, so HTTP/1.1 clients are unaffected and no endpoint changes.

Filegate never terminates TLS, so HTTPS and browser-facing HTTP/2 belong at a
reverse proxy in front of it. There is no TLS configuration to enable.

| Situation | Setting |
|---|---|
| Proxy speaks HTTP/1.1 to its backends (nginx, ingress-nginx, and the default for Caddy, Traefik and Envoy) | Leave it off. |
| Proxy or service mesh is configured to speak h2 to its backends | Turn it on. |
| Chasing throughput | Leave it off; it is not a performance setting. |

`server.http2_cleartext` is static — the protocol set is fixed once the listener
accepts connections, so changing it needs a restart. It applies to the REST
listener only. The S3 listener stays HTTP/1.1: S3 clients sign and stream over
HTTP/1.1 in practice, so h2c there would be surface without a consumer.

It is off by default. Enable it when the upstream proxy or service mesh uses h2c.

Measurements are in [HTTP/1.1 compared with h2c](/docs/en/development/benchmarks/results/h2c): across four repeats per
configuration, HTTP/1.1 and h2c differ by 1.5% to 6.7% at the median, inside a
spread that reaches 2.6x within a single arm. Enable it because a proxy requires
it, not to make anything faster.

Proxy configuration for h2c upstreams:

```caddyfile
reverse_proxy filegate:8080 {
	transport http {
		versions h2c 2
	}
}
```

```yaml
# Traefik
services:
  filegate:
    loadBalancer:
      servers:
        - url: h2c://filegate:8080
```

## Container Deployment

Use the provided Dockerfile or compose examples for local evaluation, CI smoke tests, or environments that explicitly standardize on containers. For ordinary Linux production hosts, prefer package install plus systemd.

The published image is distroless and does not ship the `btrfs` CLI. Its
`auto` detector therefore resolves to `poll`, including on btrfs bind mounts.
The separate kernel-level reflink probe still works, so automatic versioning
can use cheap clones when the mounted filesystem supports `FICLONE`. Use the
host package or a purpose-built image with `btrfs-progs` and suitable mount
access when btrfs delta detection is required.

For production, mount:

- file roots
- `/var/lib/filegate/config` — authoritative applied manifest and credentials;
  losing it loses generated API and S3 keys
- `/var/lib/filegate/index` — rebuildable metadata index; losing it requires a
  full data walk
- persistent logs (optional)

and inject token/config through env vars or mounted config file.

The published container targets Linux AMD64. Release packages are
published for Linux AMD64 and ARM64. The container runs as UID/GID `65532`; bind
mounts must be writable by that identity and must preserve `user.*` xattrs.

Run exactly one Filegate daemon against a given index path, runtime config
store, and writable mount set. Embedded Pebble databases are not shared stores,
and Filegate has no leader election or active-active replication.

## Admin App

The admin UI is shipped as a standalone SSR app in `admin/`, not as part of the
Filegate REST binary. Run it next to Filegate and give it:

- `FILEGATE_URL` — the REST API URL the admin server can reach
- `FILEGATE_TOKEN` — the Filegate bearer token kept server-side
- `ADMIN_INSTANCE_NAME` — optional instance name shown in the admin interface
- `ADMIN_TOKEN` — optional separate login token for browser users

Uploads use Filegate upload sessions with direct session tokens, so large file
and folder uploads go browser-to-Filegate after the admin app creates the
sessions. Downloads use scoped direct download URLs. If the browser reaches
Filegate at a different URL than the admin server, set `server.public_url` in
Filegate and configure CORS for the admin origin.

The Admin app is source-shipped. Build `admin/Dockerfile` at a pinned Filegate
commit or deploy the SSR app from `admin/` with
`bun install --frozen-lockfile`. Treat it as a
full-authority operator surface, put it behind TLS, and do not expose it
directly to the public internet.

## Production requirements

- Filegate is single-node and single-tenant. One REST bearer token has full
  file and configuration authority; only S3 keys provide narrower bucket
  scopes.
- Filegate listeners are cleartext. Terminate TLS and enforce network policy at
  a reverse proxy or private service boundary.
- REST has no built-in request rate limiter. Apply limits at the proxy. S3 keys
  can have per-key request limits.
- File operations publish atomically one operation at a time. There is no
  multi-file transaction and no cross-request snapshot isolation.
- External filesystem changes become visible eventually through detection and
  reconciliation. HTTP and S3 writes update the index in their write path.
- The filesystem holds file bytes, paths, and stable-ID xattrs. The runtime
  config store holds applied config and generated credentials. The metadata
  index is rebuildable but still must not be shared by running daemons.

See [System administration reference](/docs/en/reference/sysadmin) for backup, restore, upgrade, rollback, and
readiness checks.
