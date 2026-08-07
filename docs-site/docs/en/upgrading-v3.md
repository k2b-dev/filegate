---
title: Upgrade to v3
navTitle: Upgrade to v3
section: Start
order: 35
description: Update Filegate package and repository coordinates for v3.
tags: [upgrade, typescript, go, docker]
---

# Upgrade to v3

Filegate v3 is published from the `k2b-dev/filegate` repository. The namespace
move changes the TypeScript package, Go module, and container image coordinates.
The Filegate binary name, REST and S3 endpoints, configuration keys, and storage
paths are unchanged by the namespace move.

## TypeScript

Replace the package and all imports:

```sh
npm remove @valentinkolb/filegate
npm install @k2b/filegate
```

```ts
import { Filegate } from "@k2b/filegate";
import { uploads } from "@k2b/filegate/utils";
```

The public entry points remain `.`, `./client`, and `./utils`.

## Go

Filegate v3 follows Go semantic import versioning:

```sh
go get github.com/k2b-dev/filegate/v3@v3.0.0
```

Update imports to include both the new organization and `/v3`:

```go
import "github.com/k2b-dev/filegate/v3/sdk/filegate"
```

Run `go mod tidy` after replacing imports.

## Container image

Use the organization image for new deployments:

```text
ghcr.io/k2b-dev/filegate:3.0.0
```

Pin the published digest in production after verifying the release in your
environment.

## Releases and source links

Release packages and source now live at
[`k2b-dev/filegate`](https://github.com/k2b-dev/filegate). Existing Git history,
tags, issues, and releases moved with the repository; old GitHub links redirect
to the new location.
