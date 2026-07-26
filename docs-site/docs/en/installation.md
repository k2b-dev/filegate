---
title: Installation
navTitle: Installation
section: Start
order: 30
description: Install Filegate from published Linux packages and run it as a systemd service.
tags: [install, systemd, packages]
---

# Installation

This page is for operators installing Filegate on Linux hosts from published release packages.

The recommended production deployment is a Linux package managed by systemd. The package installs the `filegate` binary, a short `fg` symlink, a systemd unit, and a protected configuration file.

## Install the latest package

Choose the package format and CPU architecture that match the host.

### Debian and Ubuntu

Install the latest AMD64 package:

```sh
curl -fL -o /tmp/filegate.deb \
  https://github.com/valentinkolb/filegate/releases/latest/download/filegate_linux_amd64.deb
sudo dpkg -i /tmp/filegate.deb
```

Install the latest ARM64 package:

```sh
curl -fL -o /tmp/filegate.deb \
  https://github.com/valentinkolb/filegate/releases/latest/download/filegate_linux_arm64.deb
sudo dpkg -i /tmp/filegate.deb
```

### RHEL, Rocky, Fedora, and compatible hosts

Install the latest AMD64 package:

```sh
curl -fL -o /tmp/filegate.rpm \
  https://github.com/valentinkolb/filegate/releases/latest/download/filegate_linux_amd64.rpm
sudo rpm -Uvh /tmp/filegate.rpm
```

Install the latest ARM64 package:

```sh
curl -fL -o /tmp/filegate.rpm \
  https://github.com/valentinkolb/filegate/releases/latest/download/filegate_linux_arm64.rpm
sudo rpm -Uvh /tmp/filegate.rpm
```

## Installed files

| Artifact | Installed path | Scope | Meaning |
|---|---|---:|---|
| Binary | `/usr/bin/filegate` | Host | Full CLI and server binary. |
| Symlink | `/usr/bin/fg` | Host | Short command pointing to `/usr/bin/filegate`. |
| Config file | `/etc/filegate/conf.yaml` | Service | Main configuration file. |
| systemd unit | `/lib/systemd/system/filegate.service` | Service | Filegate service unit. |
| Data directory | `/var/lib/filegate` | Service | Default service data path. |
| Default mount | `/var/lib/filegate/data` | Storage | Served when no mount is configured. |
| Index | `/var/lib/filegate/index` | Service | Rebuildable metadata index. |
| Runtime config store | `/var/lib/filegate/config` | Service | Settings changed at runtime, S3 access keys, and the generated API token. Not rebuildable — back it up. |
| Log directory | `/var/log/filegate` | Service | Default service log path. |

The package creates or preserves `/etc/filegate/conf.yaml`. Existing config files are not overwritten during upgrades.

`fg` is a symlink installed by the package postinstall script. It is not a separate binary. An optional shell alias can also be written during package install by setting `FILEGATE_INSTALL_ALIAS_FG=1`.

## Configure the service

Set a REST bearer token before starting Filegate:

```sh
sudo fg config set --config /etc/filegate/conf.yaml \
  --auth-bearer-token '<strong-token>'
```

Add at least one storage mount:

```sh
sudo mkdir -p /srv/filegate/data
sudo fg config mount add --config /etc/filegate/conf.yaml /srv/filegate/data
```

Validate the config:

```sh
sudo fg config validate --config /etc/filegate/conf.yaml
```

## Start Filegate

```sh
sudo systemctl daemon-reload
sudo systemctl enable filegate
sudo systemctl start filegate
sudo systemctl status filegate
```

Verify the REST listener:

```sh
curl -fsS http://127.0.0.1:8080/health
```

## Behind a reverse proxy

Filegate never terminates TLS. HTTPS, and browser-facing HTTP/2, belong at a
reverse proxy in front of it; there is no TLS configuration to enable.

The listener serves cleartext HTTP/1.1. That is what every mainstream proxy speaks
to its backends — nginx does not support HTTP/2 upstreams at all, and Caddy,
Traefik and Envoy default to HTTP/1.1 — so no configuration is needed for the
usual setup.

Set `server.http2_cleartext` when your proxy or service mesh is configured to
speak h2 to its backends. HTTP/1.1 and h2c then share the same port: the server
switches only for a connection that opens with the HTTP/2 preface, so nothing else
changes. It is a static setting, so it needs a restart, and it is not a
performance setting — measurements put the two protocols within a few percent at
the median, well inside the noise of a single configuration.

```sh
sudo fg config set --config /etc/filegate/conf.yaml --server-http2-cleartext
sudo systemctl restart filegate
```

The startup log states what the listener accepted:

```txt
[filegate] listening on :8080 (HTTP/1.1, h2c)
```

The S3 listener is HTTP/1.1 only. S3 clients sign and stream over HTTP/1.1 in
practice.

## Upgrade Filegate

Stop Filegate before installing a newer package:

```sh
sudo systemctl stop filegate
```

Then install the latest package again using the same command for your distribution and architecture.

Start the service after the package upgrade:

```sh
sudo systemctl start filegate
sudo systemctl status filegate
```

The package preinstall script refuses to replace package files while `filegate.service` is active.

## Container evaluation

Use the Docker image for evaluation, CI smoke tests, or environments that intentionally standardize on containers. For ordinary Linux production hosts, use the package and systemd path above.

```sh
mkdir -p ./filegate-data

docker run --rm -d \
  --name filegate \
  -p 8080:8080 \
  -e FILEGATE_AUTH_BEARER_TOKEN=dev-token \
  -e FILEGATE_STORAGE_BASE_PATHS=/data \
  -e FILEGATE_STORAGE_INDEX_PATH=/var/lib/filegate/index \
  -v "$PWD/filegate-data:/data" \
  ghcr.io/valentinkolb/filegate:latest \
  serve
```

Beyond evaluation, mount `/var/lib/filegate/config` as a volume too. It holds the S3 access keys and every setting changed through the admin UI, and a container that recreates it starts over with none of them.

## Development build

Build from source only when developing Filegate itself:

```sh
go build -o ./bin/fg ./cmd/filegate
```
