---
title: Start a daemon
section: Start
order: 20
description: Configure a root, a bearer token and the Filegate service.
---

# Start a daemon

Filegate requires Linux and an existing writable data directory. Indexed roots
also require writable `user.*` extended attributes. The ext4, XFS, and Btrfs filesystems support
these; the mounted filesystem and service permissions determine availability.
An index-free root does not require xattrs. NFS deployments should start with
`index: false`.

## Create storage and credentials

For a package installation, use the `filegate` service account:

```sh
sudo install -d -o filegate -g filegate -m 0750 /srv/filegate/cloud /var/lib/filegate
sudo install -d -o root -g filegate -m 0750 /etc/filegate
sudo sh -c 'umask 027; openssl rand -hex 32 > /etc/filegate/token'
sudo chown root:filegate /etc/filegate/token
```

Copy the [configuration example](configuration.md) into `/etc/filegate/conf.yaml`.
Set `server.public_url` to the externally reachable origin. Direct URLs use that
origin; Filegate does not derive it from untrusted request headers.

```sh
sudo -u filegate filegate validate
sudo -u filegate filegate serve
```

A successful `validate` checks the static configuration, existing root paths and
token. Starting the daemon additionally checks exclusive state ownership,
private storage and indexed filesystem access. `GET /health` returns
`{"ready":true}` once startup completes.

## Use systemd

The package includes `filegate.service`. Its default writable paths are
`/var/lib/filegate` and `/srv/filegate`. Add other roots to a service override
before starting it:

```ini
[Service]
ReadWritePaths=/data/cloud /data/nfs
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now filegate
sudo -u filegate filegate roots
```

The package does not start the service or generate application credentials.
Review [ownership and service privileges](security.md) before enabling numeric
UID/GID changes. Terminate TLS at a reverse proxy; bind the daemon to a private
interface. Restart the service after changing configuration.
