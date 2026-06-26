---
title: Admin UI
navTitle: Admin UI
section: Operate
order: 105
description: Use the Filegate admin UI for browser-based file operations and service inspection.
tags: [admin, ui]
---

# Admin UI

The admin UI is for operators who need browser access to Filegate files, metadata, activity, and runtime state.

It runs as a separate SSR app next to Filegate. File bytes still move through Filegate; the UI keeps the Filegate bearer token on the admin server.

## Runtime model

```txt
browser <-> admin app <-> Filegate REST API
browser <-> Filegate direct upload/download URLs
```

The Filegate bearer token stays on the admin server. Browser uploads and downloads use scoped direct URLs.

## Use it for

| Page | Scope | Use for |
|---|---:|---|
| Overview | Service | Mount and storage summary. |
| Files | Mount and node | Browse, upload, download, create folders, transfer, rename, edit POSIX metadata, and delete. |
| Search | Service index | Glob search over indexed paths. |
| System | Service | Metrics, index state, cache pressure, activity, and index rescan. |

## Environment

| Variable | Scope | Required | Meaning |
|---|---:|---:|---|
| `FILEGATE_URL` | Admin server process | Yes | REST API URL reachable from the admin app. |
| `FILEGATE_TOKEN` | Admin server process | Yes | Filegate bearer token kept server-side. |
| `ADMIN_TOKEN` | Browser login | No | Separate admin login token. Defaults to `FILEGATE_TOKEN`. |
| `ADMIN_SESSION_SECRET` | Browser session cookie | No | Session signing secret. Defaults to `FILEGATE_TOKEN`. |
| `PORT` | Admin server process | No | HTTP listen port. Defaults to `3000`. |

## Start the admin app

Run the admin app where it can reach the Filegate REST API. The Filegate bearer token stays in the admin server process.

```sh
cd admin
bun install
FILEGATE_URL=http://127.0.0.1:8080 \
FILEGATE_TOKEN=dev-token \
ADMIN_TOKEN=admin-token \
bun run dev
```

Open `http://127.0.0.1:3000` and sign in with `ADMIN_TOKEN`.

## Browser transfer behavior

| Operation | Data path | Meaning |
|---|---|---|
| Upload | Browser to Filegate | Admin app creates sessions; browser sends file bytes to scoped Filegate URLs. |
| Download | Browser from Filegate | Admin app mints a scoped URL and redirects browser to it. |
| Metadata mutation | Browser to admin to Filegate | Admin app submits REST calls with the server-side bearer token. |
