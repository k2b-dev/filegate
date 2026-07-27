<p align="center">
    <img src="docs/assets/logo.svg" alt="Filegate" width="256"/>
</p>

# Filegate

Filegate is a Linux file gateway for applications that need HTTP access to normal filesystem storage.

Files stay as normal files on configured mounts. File bytes, directory layout, and stable IDs live on the filesystem; Pebble indexes path/id lookup, directory listings, and stats.

## Core properties

- **Filesystem-first storage:** data remains inspectable and recoverable with standard Linux tooling; backups and snapshots use the normal filesystem layout.
- **Indexed metadata:** hot metadata reads use Pebble instead of walking the tree; Filegate keeps reverse `path <-> id` lookup.
- **Stable file identity:** IDs are stored in `user.filegate.id` xattrs, so files can move without becoming new logical objects.
- **Explicit writes:** uploads default to conflict errors; callers opt into `overwrite` or `rename`.
- **Multiple access surfaces:** native REST API, TypeScript client, and optional path-style S3 listener.

## Requirements

- Linux for `fg serve`.
- Storage mounts with user xattr support.
- btrfs is recommended for fast change detection, reflink copies, and version history.
- ext4 and other Linux filesystems can run with poll detection.

## Production install

For production, install the package and run Filegate under systemd. Packages install `/usr/bin/filegate`, `/etc/filegate/conf.yaml`, `filegate.service`, `/var/lib/filegate`, and `/var/log/filegate`.

```bash
curl -fL -o /tmp/filegate.deb \
  https://github.com/valentinkolb/filegate/releases/latest/download/filegate_linux_amd64.deb
sudo dpkg -i /tmp/filegate.deb
# or
curl -fL -o /tmp/filegate.rpm \
  https://github.com/valentinkolb/filegate/releases/latest/download/filegate_linux_amd64.rpm
sudo rpm -Uvh /tmp/filegate.rpm
```

For ARM64 hosts, replace `amd64` with `arm64` in the package URL.

Set the initial token and start the service:

```bash
sudo fg config set --config /etc/filegate/conf.yaml \
  --auth-bearer-token '<strong-token>'

sudo systemctl enable filegate
sudo systemctl start filegate
sudo systemctl status filegate
```

Smoke test:

```bash
curl -fsS http://127.0.0.1:8080/health

fg status --config /etc/filegate/conf.yaml
```

## Local binary

```bash
go build -o ./bin/fg ./cmd/filegate
mkdir -p /tmp/filegate/data /tmp/filegate/index
```

```yaml
# conf.yaml
server:
  listen: ":8080"
  public_url: "http://127.0.0.1:8080"
  cors:
    allowed_origins: []

auth:
  bearer_token: "dev-token"

storage:
  base_paths:
    - /tmp/filegate/data
  index_path: /tmp/filegate/index
```

```bash
./bin/fg serve --config ./conf.yaml
```

The mount basename becomes the root name. `/tmp/filegate/data` is exposed as REST path `/data/...` and as S3 bucket `data` when S3 is enabled.

## Docker

Filegate can run in Docker. Use Docker for local evaluation, CI smoke tests, or container-based deployments.

```bash
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

```bash
curl -fsS http://127.0.0.1:8080/health

curl -fsS -H 'Authorization: Bearer dev-token' \
  http://127.0.0.1:8080/v1/paths/
```

## Configuration

Filegate uses a versioned desired-state manifest for repository-managed configuration. Plan and apply use the same bearer-authenticated HTTP API locally and remotely:

```yaml
# filegate.manifest.yaml
version: 1
config:
  server:
    public_url: https://files.example.com
    access_log_enabled: true
  upload:
    max_upload_bytes: 1073741824
```

```bash
fg config plan -f filegate.manifest.yaml --host https://files.example.com --token-file /run/secrets/filegate-token
fg config apply -f filegate.manifest.yaml --host https://files.example.com --token-file /run/secrets/filegate-token
```

The manifest is a complete replacement: removing a key removes it from managed state. Runtime keys apply immediately; static keys are stored as desired state until restart. The admin Settings page is a read-only view of effective and desired configuration.

Bootstrap settings and secrets still come from `--config`, `FILEGATE_CONFIG`, environment, or default candidates such as `/etc/filegate/conf.yaml`. Environment variables use `FILEGATE_` plus the config path, for example `FILEGATE_SERVER_LISTEN`.

Use the config CLI for offline edits:

```bash
fg config show --config /etc/filegate/conf.yaml
fg config validate --config /etc/filegate/conf.yaml

sudo mkdir -p /srv/filegate/photos
sudo fg config mount add --config /etc/filegate/conf.yaml /srv/filegate/photos

sudo fg config set --config /etc/filegate/conf.yaml \
  --auth-bearer-token '<strong-token>' \
  --server-listen ':8080'
```

Mutating `fg config` commands require explicit `--config`, create a timestamped backup by default, validate the resulting YAML before replacing it, and print a restart reminder. They do not hot-reload a running daemon.

`fg serve` accepts the same config-value flags as one-shot startup overrides:

```bash
fg serve --config ./conf.yaml --server-listen ':9090'
```

## REST API

All `/v1/*` routes require `Authorization: Bearer <token>` except scoped direct upload/download URLs. `/health` is public.

```bash
# List roots.
curl -fsS -H 'Authorization: Bearer dev-token' \
  http://127.0.0.1:8080/v1/paths/

# Upload by virtual path.
curl -fsS -X PUT \
  -H 'Authorization: Bearer dev-token' \
  --data-binary @photo.jpg \
  'http://127.0.0.1:8080/v1/paths/data/photo.jpg?onConflict=rename'

# Runtime stats.
curl -fsS -H 'Authorization: Bearer dev-token' \
  http://127.0.0.1:8080/v1/stats
```

REST routes cover path and ID lookup, file content, directory listings, uploads, transfers, search, thumbnails, stats, activity, capabilities, and version operations.

### Direct browser transfer URLs

For browser uploads and downloads, keep the Filegate bearer token on your application server. The server asks Filegate for a short-lived scoped URL and gives that URL to the browser.

```bash
curl -fsS -X POST \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"path":"data/inbox/photo.jpg","contentType":"image/jpeg","expiresInSeconds":900,"onConflict":"rename"}' \
  http://127.0.0.1:8080/v1/uploads/direct
```

```bash
curl -fsS -X POST \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"path":"data/inbox/photo.jpg","expiresInSeconds":300}' \
  http://127.0.0.1:8080/v1/downloads/direct
```

Direct file downloads support `GET`, `HEAD`, and byte ranges. Directory downloads stream tar archives.

Set `server.public_url` (`--server-public-url`, `FILEGATE_SERVER_PUBLIC_URL`) to the external REST URL behind Traefik/Caddy/nginx. If it is empty, Filegate builds URLs from the incoming request host.

CORS is disabled by default. Prefer configuring CORS at your reverse proxy; if Filegate must answer browser cross-origin requests directly, allow specific origins:

```bash
sudo fg config set --config /etc/filegate/conf.yaml \
  --server-cors-allowed-origins 'https://app.example.com' \
  --server-cors-exposed-headers X-Node-Id \
  --server-cors-exposed-headers X-Created-Id
```

## S3 listener

S3 is disabled by default. Enable it, add a key, restart Filegate, and configure clients for path-style addressing.

```bash
sudo fg config set --config /etc/filegate/conf.yaml \
  --s3-enabled \
  --s3-listen ':9100' \
  --s3-region us-east-1

sudo fg config s3 key add --config /etc/filegate/conf.yaml \
  --all-buckets

sudo systemctl restart filegate
```

The key command prints the generated `access_key` and `secret_key` once. Store them as secrets.

```bash
aws --endpoint-url http://127.0.0.1:9100 s3 ls s3://data/
```

Each mount becomes one bucket named after the path basename. CreateBucket and DeleteBucket are rejected; buckets are operator-configured mounts.

Large folder uploads stage multipart parts under `<mount>/.fg-uploads` until completion. Use a real volume or bind mount with enough free space, and lower concurrent S3 writes on small disks or low `ulimit -n` systems:

```bash
sudo fg config set --config /etc/filegate/conf.yaml \
  --s3-max-concurrent-writes 32
```

## TypeScript client

```bash
npm i @valentinkolb/filegate
```

```ts
import { filegate } from "@valentinkolb/filegate/client";

process.env.FILEGATE_URL = "http://127.0.0.1:8080";
process.env.FILEGATE_TOKEN = "dev-token";

const roots = await filegate.paths.get();
if ("items" in roots) {
  console.log(roots.items.map((node) => `${node.name} ${node.id}`));
}
```

For dependency injection:

```ts
import { Filegate } from "@valentinkolb/filegate/client";

const fg = new Filegate({
  baseUrl: "https://filegate.internal.example",
  token: "<filegate-token>",
});
```

The client namespaces match the API surface: `paths`, `nodes`, `uploads`, `downloads`, `transfers`, `search`, `index`, `stats`, `capabilities`, `versions`, and `activity`.

```ts
const caps = await fg.capabilities.get();
```

Do not put the Filegate bearer token in browser bundles. For single-file direct browser uploads, mint a short-lived URL on your backend and upload with the token-free helper:

```ts
import { uploadDirect } from "@valentinkolb/filegate/client";

await uploadDirect(uploadUrlFromYourBackend, file, {
  onSuccess: async ({ node }) => {
    await fetch("/api/uploads/complete", {
      method: "POST",
      body: JSON.stringify({ filegateId: node.id }),
    });
  },
  onError: async (error) => {
    await fetch("/api/uploads/failed", { method: "POST", body: String(error) });
  },
  onFinish: async () => {
    console.log("upload attempt finished");
  },
});
```

For multi-file browser uploads, use the high-level upload helper. Your application server receives one allow request, applies auth and policy, and returns direct one-shot uploads or direct upload sessions.

```ts
import { upload } from "@valentinkolb/filegate/client";

await upload({
  files,
  path: "data/inbox",
  allow: async (request) => {
    const response = await fetch("/api/filegate/uploads/allow", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    });
    return response.json();
  },
  onEvent: (event) => console.log(event.type),
});
```

For direct browser downloads, mint a URL on your backend and redirect the browser:

```ts
const direct = await fg.downloads.createDirectURL({
  nodeId: "<node-id>",
  expiresInSeconds: 5 * 60,
});

return Response.redirect(direct.downloadUrl, 303);
```

## Admin app

The admin app is an SSR UI in `admin/`. Run it next to Filegate for file operations, metrics, activity, and index operations without putting the Filegate bearer token in the browser.

```bash
cd admin
bun install

FILEGATE_URL=http://127.0.0.1:8080 \
FILEGATE_TOKEN=dev-token \
ADMIN_TOKEN=admin-token \
bun run dev
```

Browser uploads and downloads use scoped direct Filegate URLs. Metadata mutations go through the admin app server.

## Bun S3Client

Bun's native `S3Client` works with Filegate's path-style S3 endpoint.

```ts
import { S3Client } from "bun";

const s3 = new S3Client({
  accessKeyId: process.env.FILEGATE_S3_ACCESS_KEY!,
  secretAccessKey: process.env.FILEGATE_S3_SECRET_KEY!,
  region: "us-east-1",
  endpoint: "http://127.0.0.1:9100",
  bucket: "data",
});
```

```ts
await s3.write("notes/hello.txt", "hello from Bun\n", {
  type: "text/plain",
});

const text = await s3.file("notes/hello.txt").text();
const stat = await s3.stat("notes/hello.txt");
const firstFive = await s3.file("notes/hello.txt").slice(0, 5).text();

const page = await s3.list({ prefix: "notes/" });
const keys = (page.contents ?? []).map((item) => item.key);

await s3.delete("notes/hello.txt");
```

For large listings, use a client path that exposes S3 continuation tokens. Filegate paginates S3 listings at the normal page boundary.

## Package upgrades

The package does not auto-start the service. Package upgrades are offline: stop `filegate` before installing a new package. The preinstall script refuses to replace package files while `filegate.service` is active.

```bash
sudo systemctl stop filegate
curl -fL -o /tmp/filegate.deb \
  https://github.com/valentinkolb/filegate/releases/latest/download/filegate_linux_amd64.deb
sudo dpkg -i /tmp/filegate.deb
sudo systemctl start filegate
```

## Operations

```bash
fg health --config /etc/filegate/conf.yaml
fg status --config /etc/filegate/conf.yaml
fg index stats --config /etc/filegate/conf.yaml
fg index rescan --config /etc/filegate/conf.yaml
```

Use `index rescan --new` only with the daemon stopped:

```bash
sudo systemctl stop filegate
sudo fg index rescan --new --config /etc/filegate/conf.yaml
sudo systemctl start filegate
```

Prometheus metrics are optional and served on the REST listener:

```bash
sudo fg config set --config /etc/filegate/conf.yaml \
  --metrics-enabled \
  --metrics-path /metrics
```

Recent API activity is kept in a bounded in-memory ring buffer and exposed through `/v1/activity`:

```bash
curl -fsS -H 'Authorization: Bearer dev-token' \
  'http://127.0.0.1:8080/v1/activity?limit=20'
```

The default ring buffer retains 500 records. Set `activity.ring_buffer_size` to adjust it.

## Limits

- Single-node service; no replication.
- Config changes are offline; restart after editing config.
- REST uses one bearer token. S3 supports multiple keys and per-key bucket allowlists.
- REST has no request rate limiting. S3 supports per-key request limits.
- `X-Forwarded-For` is trusted only from configured `server.trusted_proxies`.
- `X-Filegate-Actor` can attach delegated actor context to activity records.
- S3 is path-style only.
- External filesystem changes are reconciled eventually by the detector or by a manual rescan.
- Version history is REST-side and not exposed as S3 object versioning.
- OpenTelemetry tracing is not implemented.

## More docs

- CLI: [docs/cli.md](https://github.com/ValentinKolb/filegate/blob/main/docs/cli.md)
- REST routes: [docs/http-routes.md](https://github.com/ValentinKolb/filegate/blob/main/docs/http-routes.md)
- S3 API: [docs/s3-api.md](https://github.com/ValentinKolb/filegate/blob/main/docs/s3-api.md)
- S3 config and clients: [docs/s3-config.md](https://github.com/ValentinKolb/filegate/blob/main/docs/s3-config.md), [docs/s3-clients.md](https://github.com/ValentinKolb/filegate/blob/main/docs/s3-clients.md)
- TypeScript client: [docs/ts-client.md](https://github.com/ValentinKolb/filegate/blob/main/docs/ts-client.md)
- Metrics: [docs/metrics.md](https://github.com/ValentinKolb/filegate/blob/main/docs/metrics.md)
- Versioning: [docs/versioning.md](https://github.com/ValentinKolb/filegate/blob/main/docs/versioning.md)
- Deployment and sysadmin: [docs/deployment.md](https://github.com/ValentinKolb/filegate/blob/main/docs/deployment.md), [docs/sysadmin.md](https://github.com/ValentinKolb/filegate/blob/main/docs/sysadmin.md)
- Architecture and behavior: [docs/architecture.md](https://github.com/ValentinKolb/filegate/blob/main/docs/architecture.md), [docs/behavior-and-assumptions.md](https://github.com/ValentinKolb/filegate/blob/main/docs/behavior-and-assumptions.md)

## Development

```bash
go test ./...
go vet ./...
staticcheck ./...
```

## Agent skills

```bash
bunx skills add ValentinKolb/filegate
```

- User/API skill: [skills/filegate/SKILL.md](https://github.com/ValentinKolb/filegate/blob/main/skills/filegate/SKILL.md)
- Repo engineering skill: [skills/filegate-dev/SKILL.md](https://github.com/ValentinKolb/filegate/blob/main/skills/filegate-dev/SKILL.md)

## License

MIT, see [LICENSE](https://github.com/ValentinKolb/filegate/blob/main/LICENSE).
