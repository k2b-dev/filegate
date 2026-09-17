---
title: Start a daemon
section: Start
order: 20
description: Configure a root, a bearer token and the Filegate service.
---

# Start a daemon

Filegate requires Linux and an existing writable data directory. Indexed roots
also require writable `user.*` extended attributes (xattrs), supported by ext4,
XFS and Btrfs. Check their availability with the actual mount and service
permissions. With `index: false`, Filegate can serve files without xattr support.

## Install a Linux package

Download the package from [GitHub Releases](https://github.com/k2b-dev/filegate/releases)
and install it with your system's package manager. Packages include the CLI,
`filegate.service`, a service account and an example configuration at
`/etc/filegate/conf.yaml`.

The commands below install **v5.0.0**.
For ARM64 systems, replace `amd64` with `arm64` in both the URL and filename.

### Debian

```sh
curl -fLO https://github.com/k2b-dev/filegate/releases/download/v5.0.0/filegate_linux_amd64.deb
sudo apt install ./filegate_linux_amd64.deb
```

### Rocky Linux

```sh
curl -fLO https://github.com/k2b-dev/filegate/releases/download/v5.0.0/filegate_linux_amd64.rpm
sudo dnf install ./filegate_linux_amd64.rpm
```

The service starts after you configure storage and credentials and enable it
with systemd. For upgrades, stop it with `sudo systemctl stop filegate` before
installing the replacement package.

## Create storage and credentials

For a package installation, use the `filegate` service account:

```sh
sudo install -d -o filegate -g filegate -m 0750 /srv/filegate/documents /var/lib/filegate
sudo install -d -o root -g filegate -m 0750 /etc/filegate
sudo sh -c 'umask 027; openssl rand -hex 32 > /etc/filegate/token'
sudo chown root:filegate /etc/filegate/token
```

Copy the [configuration example](/docs/en/configuration) into `/etc/filegate/conf.yaml`.
If you include the `shared` root, mount its storage at `/mnt/shared` first;
otherwise remove that entry. Set `server.public_url` to the externally reachable
origin used for direct URLs.

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
before starting it. For the optional `shared` root:

```ini
[Service]
ReadWritePaths=/mnt/shared
```

Stop the foreground daemon with Ctrl+C before starting the service:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now filegate
sudo -u filegate filegate roots
```

The package does not start the service or generate application credentials.
Review [ownership and service privileges](/docs/en/security) before enabling numeric
UID/GID changes. Terminate TLS at a reverse proxy; bind the daemon to a private
interface. Restart the service after changing configuration.
