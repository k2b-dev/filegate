---
title: Go SDK
navTitle: Go SDK
section: APIs
order: 90
description: Use the Go SDK for server-side Filegate integrations.
tags: [go, sdk]
---

# Go SDK

The Go SDK is for Go services and tools that call Filegate over HTTP.

## Client setup

```go
package main

import (
	"context"
	"log"

	"github.com/k2b-dev/filegate/v3/sdk/filegate"
)

func main() {
	client, err := filegate.New(filegate.Config{
		BaseURL: "http://127.0.0.1:8080",
		Token:   "dev-token",
	})
	if err != nil {
		log.Fatal(err)
	}

	roots, err := client.Paths.List(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	log.Println(roots)
}
```

## Main packages

| Package | Scope | Use for |
|---|---:|---|
| `sdk/filegate` | Go process | Main HTTP client and typed API methods. |
| `sdk/filegate/directuploads` | Browser or external direct upload helpers | Signed direct upload flows. |
| `sdk/filegate/segments` | Upload segment planning | Segment math and checksum-related helpers. |
| `sdk/filegate/relay` | Application server relay patterns | Server-side helpers for proxying or authorizing browser transfers. |
| `sdk/filegate/uploadtree` | Whole folders | Batch uploads over many one-file sessions. |

## Uploading a folder

`sdk/filegate/uploadtree` is the Go counterpart to the browser `upload()` helper
in the TypeScript SDK. It walks a local directory, hashes with bounded
concurrency, creates sessions in batches, uploads segments under one global
concurrency limit, retries transient failures, and reports one progress view for
the whole run.

```go
sources, err := uploadtree.FromDir("/srv/photos", "data/photos")
if err != nil {
	log.Fatal(err)
}

res, err := uploadtree.Upload(ctx, client, sources, uploadtree.Options{
	OnConflict:  filegate.ConflictOverwrite,
	Resume:      true, // adopt sessions an interrupted run left behind
	Concurrency: uploadtree.Concurrency{Hash: 4, Files: 8, Segments: 8},
	OnEvent: func(e uploadtree.Event) {
		if e.Type == uploadtree.EventFileDone {
			log.Printf("%d/%d %s", e.Progress.FilesDone, e.Progress.Files, e.Path)
		}
	},
})
if err != nil {
	log.Fatal(err)
}
log.Printf("done=%d failed=%d skipped=%d", res.Done, res.Failed, res.Skipped)
```

A single file failing does not stop the run; its error is in `res.Files`.

### Small files skip the session protocol

Files at or below `DirectThresholdBytes` take one `PUT /v1/paths` instead of
create, segment and commit. It defaults to `SegmentSize`, so a file that would
have been a single segment goes direct without any configuration.

The default avoids three session round trips and several fsyncs for files that
fit in one segment. Measurements in
[Upload commit measurements](/docs/en/development/benchmarks/results/commit-cost) put the one-shot path at 2.4x the
throughput of sessions on a 5000-file corpus averaging 16 KiB.

Set `DirectThresholdBytes` to a negative value to send everything through
sessions; `0` means "use the default", because `Options`' zero value has to stay
valid. The TypeScript `upload()` helper takes the same default and uses `0` as
its opt-out.

## API coverage

| Namespace | Methods |
|---|---|
| `Paths` | `List`, `Get`, `Put`, `PutRaw` |
| `Nodes` | `Get`, `ContentRaw`, `PipeContent`, `PutContent`, `Mkdir`, `Patch`, `Delete`, `ThumbnailRaw` |
| `Uploads` | `CreateDirectUploadURL`; sessions: `Create`, `CreateBatch`, `Status`, `PutSegment`, `PutSegmentRaw`, `Commit`, `Abort` |
| `Downloads` | `CreateDirectURL` |
| `Transfers` | `Create` |
| `Search` | `Glob` |
| `Index` | `Rescan`, `ResolvePath`, `ResolvePaths`, `ResolveID`, `ResolveIDs` |
| `Stats` | `Get` |
| `System` | `Info`, `Runtime`, `Health`, `Prune`, `UploadSessions` |
| `Config` | `Schema`, `Values`, `Plan`, `Apply` |
| `S3Keys` | `List`, `Create`, `Update`, `Rotate`, `Delete` |
| `Capabilities` | `Get` |
| `Versions` | `List`, `ListAll`, `ContentRaw`, `PipeContent`, `Snapshot`, `Pin`, `Unpin`, `Restore`, `Delete` |
| `Activity` | `List` |

## Configuration and S3 administration

Plan complete desired state before applying it with the observed revision:

```go
desired := map[string]any{"metrics.enabled": true}
plan, err := client.Config.Plan(ctx, desired)
if err != nil {
	return err
}
if len(plan.Changes) > 0 {
	_, err = client.Config.Apply(ctx, desired, plan.CurrentRevision)
}
```

S3 key secrets are returned only by create and rotate:

```go
created, err := client.S3Keys.Create(ctx, filegate.S3KeyCreateRequest{
	Buckets: []string{"data"},
})
if err != nil {
	return err
}
fmt.Println(created.AccessKey, created.SecretKey)

rotated, err := client.S3Keys.Rotate(ctx, created.AccessKey)
```

Operational state and manual retention are under `System`:

```go
health, err := client.System.Health(ctx)
runtime, err := client.System.Runtime(ctx)
sessions, err := client.System.UploadSessions(ctx, "in_progress")
pruned, err := client.System.Prune(ctx)
```

See [HTTP routes reference](/docs/en/reference/http-routes) for the underlying REST contract.
