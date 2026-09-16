---
title: Filegate
section: Start
order: 10
description: Serve Linux files through independent roots with optional indexing and history.
---

# Filegate

Filegate serves files from Linux directories through an HTTP API, with
TypeScript and Go clients for application backends. It supports file browsing,
search, direct uploads, downloads and version history. Your application
authenticates users and controls their access to files.

A **root** is a directory made available under a configured name. Requests
identify files by root name and relative path, for example root `documents`
and path `reports/annual.pdf`. Each root has its own index and version settings.

The optional metadata index supports filename search and stable file IDs.
Filegate updates the index after API writes. Run a rebuild to include changes
made outside Filegate. With indexing disabled, search reads the filesystem.
Directory listings and file metadata always read the filesystem.

Version history requires indexing. It preserves earlier file contents and
supports manual snapshots, restore and configurable retention.

- [Configure and start a daemon](/docs/en/getting-started)
- [Choose roots and index settings](/docs/en/configuration)
- [Integrate TypeScript](/docs/en/ts-sdk) or [Go](/docs/en/go-sdk)
- [Use direct uploads](/docs/en/uploads-downloads)
- [Set version retention](/docs/en/versioning)
- [Operate and back up](/docs/en/operations)
