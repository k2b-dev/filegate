---
title: Filegate
navTitle: Overview
section: Start
order: 10
description: Filegate exposes normal Linux files through REST, SDK, and S3-compatible APIs.
tags: [overview, architecture]
---

# Filegate

Filegate is the file layer between your application and Linux storage.

> **Beta.** Filegate is single-tenant, authorizes the REST API with one all-or-nothing bearer token, and expects to run on a trusted network behind a reverse proxy. Read [Security model](security) before deciding where to put it.

Your application keeps its business logic, users, permissions, and product model. Filegate handles file APIs, indexed metadata, resumable uploads, direct browser transfers, activity records, and optional S3-compatible access.

The filesystem is the durable storage layer. Files stay as regular Linux files in the directories you configure, so standard Linux tools, backups, snapshots, rsync, and future systems can read the same data without a Filegate-specific export step.

For application UX, file metadata must be fast and predictable. Filegate keeps a metadata index next to the files so directory listings, path lookups, ID lookups, and common file metadata such as size and modified time are served without walking the filesystem for every request. The index accelerates Filegate; it does not own the file bytes. If the index is lost or stale, Filegate can rescan the configured mounts and rebuild it from the filesystem state.

The index also acts as a reverse lookup layer: applications can resolve a path to an ID, resolve an ID back to the current path, and keep using the same ID after a file is moved. The index is backed by Pebble, an embedded key-value store stored at `storage.index_path`.

## App file layer

Filegate is designed for applications that need files to behave like product objects: addressable by API, fast to list, safe to upload, and still stored as normal files. The app backend decides who can do what; Filegate executes file operations and lets the frontend use scoped direct URLs for high-throughput transfers.

```txt
                       +----------------------+
                       | Frontend             |-----------------------+
                       | web, mobile, desktop |                       |
                       +----------+-----------+                       |
                                  |                                   | scoped direct operations
                                  | app API                           | signed uploads, downloads,
                                  v                                   | upload segments
                       +----------------------+                       |
                       | App backend          |                       |
                       | business logic, auth |                       |
                       +----------+-----------+                       |
                                  |                                   |
                                  | Filegate bearer token             |
                                  v                                   |
                       +----------------------+                       |
                       | Filegate             |<----------------------+
                       | REST, SDK, S3        |
                       +----+------------+----+
                            |            |
             file bytes and |            | metadata, lookup,
             directories    |            | sessions, activity
                            v            v
          +------------------------+  +------------------------+
          | FS mounts              |  | Metadata index         |
          | source of truth        |  | fast listings, lookup  |
          | /srv/cloud/photos      |  | path <-> ID, size      |
          | /srv/cloud/documents   |  | mtime, versions        |
          +------------------------+  +------------------------+
```

## What this means

- You can inspect, back up, copy, or recover stored data without a Filegate-specific export step.
- Directory listings and file metadata reads avoid repeated filesystem walks in the common path.
- Stable node IDs are stored with filesystem metadata when the mount supports user xattrs and remain valid across moves.
- Path-to-ID and ID-to-path lookup are indexed operations, which keeps application code independent from current filenames.
- Applications can keep their own authorization and product logic while delegating file transfer, lookup, versioning, and S3-compatible access to Filegate.

This differs from systems that store user files as opaque blobs, chunk stores, or internal layouts that are hard to use without the original service. Filegate keeps the file tree readable by the host.

## Access surfaces

| Surface | Primary user | Use for |
|---|---|---|
| REST API | Application servers and tools | Full Filegate feature set. |
| TypeScript SDK | Node, Bun, browser-assisted apps | REST calls, direct browser uploads, downloads, and typed models. |
| Go SDK | Go services and tools | REST calls, uploads, downloads, and direct URL workflows. |
| S3 API | Backup and object-storage clients | Path-style S3 access to configured mounts. |
| Admin app | Operators | Browse files, upload/download, inspect metadata, manage versions, inspect activity and metrics. |

## Required platform

| Requirement | Scope | Meaning |
|---|---:|---|
| Linux | `fg serve` host | The service uses Linux filesystem behavior and xattrs. |
| User xattrs | Each storage mount | Required for stable node IDs. |
| Persistent index path | Per service | Stores the Pebble index and upload/session metadata. |
| btrfs | Per mount, optional | Enables fast change detection, reflink copies, and file versioning. |
| ext4 or other Linux filesystems | Per mount | Supported with polling change detection and without btrfs-only features. |

## Read next

- [Getting started](getting-started) gets one local service running and uploads a file.
- [Use Filegate in an app](application-architecture) shows the recommended application architecture with direct signed uploads and downloads.
- [Configuration](configuration) explains how Filegate resolves and validates configuration.
- [HTTP API](http-api) documents the REST surface.
- [Uploads and downloads](uploads-downloads) describes one-shot, resumable, and direct browser transfer patterns.
