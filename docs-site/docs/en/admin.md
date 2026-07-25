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
| `ADMIN_TOKEN` | Browser login | Yes | Admin login token. Must differ from `FILEGATE_TOKEN`. |
| `ADMIN_SESSION_SECRET` | Browser session cookie | No | Session signing secret. Generated at boot when unset; sessions then survive neither a restart nor a second replica. |
| `PORT` | Admin server process | No | HTTP listen port. Defaults to `3000`. |
| `ADMIN_TRUST_PROXY` | Rate limiting | No | Set when a reverse proxy fronts the admin app, so `X-Forwarded-For` identifies the client instead of the socket address. |
| `ADMIN_COOKIE_SECURE` | Browser session cookie | No | `auto` (default), `true` or `false`. Auto sets `Secure` unless the request host is localhost. |
| `REDIS_URL` | Rate limiting | No | Shares the login rate limit across replicas. In-memory when unset. |

`ADMIN_TOKEN` is required and must differ from `FILEGATE_TOKEN`. It previously
defaulted to it, which meant brute-forcing the admin login yielded the Filegate
master token; startup now refuses that configuration.

## Login and sessions

Sign-in issues a stateless signed session cookie holding subject, label and
expiry, verified server-side on every request. `POST /login` is rate limited to
10 attempts per 5 minutes per client.

Behind a TLS-terminating ingress the admin process sees plain HTTP, so the
`Secure` cookie flag is driven by `ADMIN_COOKIE_SECURE` rather than by the
observed request protocol. The default marks the cookie `Secure` everywhere
except localhost.

For shared rate limiting across replicas, set `REDIS_URL` in the process
environment before launch; the Redis connection is resolved at startup and not
re-read afterwards.

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
