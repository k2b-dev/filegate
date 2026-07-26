---
title: Getting started
navTitle: Getting started
section: Start
order: 20
description: Start a disposable Filegate instance, upload a file, and list it through the REST API.
tags: [quickstart, rest]
---

# Getting started

This guide is for technical users who want a short first run before installing Filegate on a Linux host.

## Prerequisites

| Requirement | Scope | Example |
|---|---:|---|
| Linux host or Linux container | Filegate service | `fg serve` runs on Linux. |
| Writable data directory | Storage mount | `/tmp/filegate/data` |
| Writable index directory | Service metadata | `/tmp/filegate/index` |
| Bearer token | REST API | `dev-token` |

None of these are strictly required — `fg serve` with no configuration at all starts, serves `/var/lib/filegate/data`, and prints a generated API token once. This guide sets them explicitly so the commands below are copy-pasteable.

## Start a disposable instance

Docker keeps the first run self-contained. For production Linux hosts, install the `.deb` or `.rpm` package and run Filegate through systemd; see [Installation](installation).

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

Check the service:

```sh
curl -fsS http://127.0.0.1:8080/health
```

## Upload a file

```sh
printf 'hello\n' > hello.txt

curl -fsS -X PUT \
  -H 'Authorization: Bearer dev-token' \
  --data-binary @hello.txt \
  'http://127.0.0.1:8080/v1/paths/data/hello.txt'
```

The root path is the basename of the configured mount. A mount at `/data` is exposed as `/data/...`.

## List the mount

```sh
curl -fsS -H 'Authorization: Bearer dev-token' \
  'http://127.0.0.1:8080/v1/paths/data'
```

Expected result shape:

```json
{
  "id": "...",
  "type": "directory",
  "name": "data",
  "path": "/data",
  "children": [
    {
      "type": "file",
      "name": "hello.txt",
      "path": "/data/hello.txt"
    }
  ]
}
```

## Stop the container

```sh
docker stop filegate
```

## Next steps

| Task | Page |
|---|---|
| Install Filegate on a Linux host | [Installation](installation) |
| Understand the application architecture | [Use Filegate in an app](application-architecture) |
| Configure mounts, uploads, metrics, and S3 | [Configuration](configuration) |
| Build browser uploads with signed URLs | [Uploads and downloads](uploads-downloads) |
| Use S3 clients | [S3 compatibility](s3) |
