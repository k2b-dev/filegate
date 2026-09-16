# Filegate

Filegate exposes ordinary Linux directories through a root-scoped HTTP API.
Run one Go binary as a systemd service. Use its CLI for administration and
its TypeScript or Go client from your application backend.

- Independent named roots; no overlapping directory trees.
- Optional metadata index, updated by API writes and an explicit rebuild.
- Optional version history on indexed roots: cooldown, manual snapshots,
  pinned versions, bounded JSON metadata, calendar retention.
- Small direct PUT uploads and resumable direct upload sessions, including
  numeric Unix ownership selected by the trusted backend.
- Directory browsing, bounded search, thumbnails, TAR downloads and transfers.
- Dashboard metadata through the API.

The application authenticates users and authorizes their file access. Filegate
addresses files by root name and relative path, with optional numeric ownership.
A root without an index reads the filesystem directly and requires no xattrs.
Indexed roots use `user.filegate.id` to keep file identities across API renames.
Version snapshots use reflinks when available and byte copies otherwise.

## Install a Linux package

Download the package from [GitHub Releases](https://github.com/k2b-dev/filegate/releases)
and install it with your system's package manager. Packages include the CLI,
`filegate.service`, a service account and an example configuration at
`/etc/filegate/conf.yaml`.

The commands below install **v4.0.0**.
For ARM64 systems, replace `amd64` with `arm64` in both the URL and filename.

### Debian

```sh
curl -fLO https://github.com/k2b-dev/filegate/releases/download/v4.0.0/filegate_linux_amd64.deb
sudo apt install ./filegate_linux_amd64.deb
```

### Rocky Linux

```sh
curl -fLO https://github.com/k2b-dev/filegate/releases/download/v4.0.0/filegate_linux_amd64.rpm
sudo dnf install ./filegate_linux_amd64.rpm
```

The service starts after you configure storage and credentials and enable it
with systemd. For upgrades, stop it with `sudo systemctl stop filegate` before
installing the replacement package.

## Run

Follow the [setup guide](https://filegate.dev/docs/en/getting-started) to configure
storage and credentials. Create the data roots and a private token file containing
at least 32 random characters. Keep the state directory outside all roots.

```sh
sudo -u filegate filegate validate --config /etc/filegate/conf.yaml
sudo systemctl enable --now filegate
sudo -u filegate filegate roots
```

Configuration changes require a restart. Administrative commands call the
running daemon; they never open its state database. Only the daemon writes state.

## Integrate

```ts
import { Filegate } from "@k2b/filegate";

const files = new Filegate({
  baseUrl: process.env.FILEGATE_URL!,
  token: process.env.FILEGATE_TOKEN!,
});
const upload = await files.root("documents").directUpload("homes/alex/notes.txt", 5, {
  metadata: { message: "First draft" },
});
// Return upload.url to the authorized browser. It sends exactly 5 bytes by PUT.
```

See [documentation](https://filegate.dev/docs/en/), [Go client](sdk/filegate),
and the portable [Filegate skill](skills/filegate/SKILL.md).

## Develop

```sh
make test                 # native Go, SDK tests and builds
make test-linux           # Linux filesystem and HTTP integration tests in Docker
make test-race            # on Linux: Go race suite
make docs                # Fibel typecheck and production build
```

The daemon requires Linux; SDKs and configuration validation also build elsewhere.
No external database is required. Back up roots, xattrs, private version blobs,
and `state_dir` together while the daemon is stopped.
