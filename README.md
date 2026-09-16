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
- Dashboard metadata through the API. No bundled admin application.

The application owns user permissions and business rules. Filegate knows roots,
relative paths and optional numeric ownership. It does not know FreeIPA or Cloud.
A root without an index reads the filesystem directly and requires no xattrs.
Indexed roots use `user.filegate.id` to keep file identities across API renames.
Version snapshots use reflinks when available and byte copies otherwise.

This is a hard API and configuration cut. There are no S3, automatic external
change detectors, runtime configuration APIs or compatibility endpoints.
The Cloud application is not part of this change.

## Run

Use [the example configuration](packaging/config/conf.yaml). Create its data
roots and a private token file containing at least 32 random characters.
Keep the state directory outside all roots.

```sh
filegate validate --config /etc/filegate/conf.yaml
filegate serve --config /etc/filegate/conf.yaml
filegate roots
filegate rebuild cloud
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
const upload = await files.root("cloud").directUpload("homes/alex/notes.txt", 5, {
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
