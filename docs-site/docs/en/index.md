---
title: Filegate
section: Start
order: 10
description: Serve Linux files through independent roots with optional indexing and history.
---

# Filegate

Filegate is a Linux file gateway for application backends. One daemon serves
ordinary directories through HTTP. Your application owns users, groups,
authorization and presentation. Filegate owns file operations and transfers.

A **root** is an independently configured directory, such as `cloud` at
`/data/cloud` or `shared` at `/data/nfs`. Requests use the root name and a relative
path. Their shared `/data` parent has no meaning to Filegate. Roots cannot overlap.

Enable the metadata index for roots primarily written through Filegate. It
accelerates search and provides stable file IDs. API writes update it; external
changes require a manual rebuild. Leave indexing off for externally managed
files: listing, stat and bounded search read the filesystem directly.

Versioning is optional and requires indexing. It works with ordinary Linux
filesystems, using reflinks when supported and byte copies otherwise. There is
no Btrfs dependency, detector, S3 endpoint, dynamic configuration API or admin UI.

- [Configure and start a daemon](getting-started.md)
- [Choose roots and index settings](configuration.md)
- [Integrate TypeScript](ts-sdk.md) or [Go](go-sdk.md)
- [Use direct uploads](uploads-downloads.md)
- [Set version retention](versioning.md)
- [Operate and back up](operations.md)
