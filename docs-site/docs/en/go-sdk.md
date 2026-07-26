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

	"github.com/valentinkolb/filegate/sdk/filegate"
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
	OnConflict:           filegate.ConflictOverwrite,
	DirectThresholdBytes: 256 << 10, // small files skip the session protocol
	Resume:               true,      // adopt sessions an interrupted run left behind
	Concurrency:          uploadtree.Concurrency{Hash: 4, Files: 8, Segments: 8},
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
`DirectThresholdBytes` matters more than it looks: measurements in
`bench/results/` show a session costs roughly 3 round trips and several fsyncs
per file, so small files are much faster as one-shot `PUT /v1/paths` uploads.

## API coverage

| Area | Scope | Supported by Go SDK |
|---|---:|---|
| Paths | Virtual path | Yes |
| Nodes | Stable node ID | Yes |
| Uploads | One-shot, sessions, direct URLs | Yes |
| Downloads | Content and direct URLs | Yes |
| Transfers | Node move/copy | Yes |
| Search | Indexed glob search | Yes |
| Index | Rescan and path/ID resolution | Yes |
| Versions | Per-file version operations | Yes |
| Stats | Service runtime state | Yes |
| Activity | In-memory activity log | Yes |
| Capabilities | Runtime upload limits | Yes |

See [HTTP routes reference](reference/http-routes) for the underlying REST contract.
