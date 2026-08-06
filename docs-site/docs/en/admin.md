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
| Files | Mount and node | Browse, sort, filter, thumbnails, upload, download, create folders, transfer, rename, edit POSIX metadata, bulk actions, and delete. Per-file version history with snapshot, restore, pin and delete. |
| S3 | Service | Listener identity, access keys, retained object activity, and read-only S3 configuration. |
| Search | Service index | Glob search over indexed paths. |
| System | Service | Live health, change detection, queue saturation, cache hit ratios, version retention with manual prune, upload sessions with abort, build versions, activity, and index rescan. |
| Settings | Service | Effective and desired configuration, provenance, manifest identity, and pending restart requirements. |

The System page polls and patches values in place. It is server-rendered first, so it is complete without JavaScript; the poll only keeps it current, and it shows a stale banner rather than frozen numbers when Filegate stops answering.

The Settings page is read-only, but the admin app still holds the Filegate bearer token. File operations and S3 resource actions therefore carry its full authority. See [Security model](/docs/en/security) for what admin access actually grants.

## Environment

| Variable | Scope | Required | Meaning |
|---|---:|---:|---|
| `FILEGATE_URL` | Admin server process | Yes | REST API URL reachable from the admin app. |
| `FILEGATE_TOKEN` | Admin server process | Yes | Filegate bearer token kept server-side. |
| `ADMIN_INSTANCE_NAME` | Admin interface | No | Instance name shown in the shell, login page and browser title. Defaults to `Filegate Admin`. |
| `ADMIN_TOKEN` | Browser login | See note | Admin login token. Must differ from `FILEGATE_TOKEN`. Required unless OIDC is configured. |
| `ADMIN_SESSION_SECRET` | Browser session cookie | No | Session signing secret. Generated at boot when unset; sessions then survive neither a restart nor a second replica. |
| `PORT` | Admin server process | No | HTTP listen port. Defaults to `3000`. |
| `ADMIN_TRUST_PROXY` | Rate limiting | No | Set when a reverse proxy fronts the admin app, so `X-Forwarded-For` identifies the client instead of the socket address. |
| `ADMIN_COOKIE_SECURE` | Browser session cookie | No | `auto` (default), `true` or `false`. Auto sets `Secure` unless the request host is localhost. |
| `REDIS_URL` | Rate limiting | No | Shares the login rate limit across replicas. In-memory when unset. |

`ADMIN_TOKEN` must differ from `FILEGATE_TOKEN`; startup rejects identical
values. This keeps the Admin login credential separate from the Filegate bearer
token.

## Single sign-on

The admin app supports OIDC single sign-on with the authorization code flow and
PKCE. Setting these four together enables it; leave them unset for token login.

| Variable | Required | Meaning |
|---|---:|---|
| `OIDC_ISSUER` | Yes | Issuer URL. Discovery reads `<issuer>/.well-known/openid-configuration`. Must use https outside localhost. |
| `OIDC_CLIENT_ID` | Yes | Client id. |
| `OIDC_CLIENT_SECRET` | Yes | Client secret, kept server-side. |
| `OIDC_REDIRECT_URL` | Yes | Must match the client's configured redirect URI and end in `/auth/callback`. |
| `OIDC_SCOPES` | No | Defaults to `openid profile email`. |
| `OIDC_GROUPS_CLAIM` | No | Claim carrying group membership. Defaults to `groups`. |
| `OIDC_ALLOWED_GROUPS` | No | Comma-separated group allowlist. |

The ID token is verified against the provider's JWKS with issuer, audience and
nonce all checked. Sessions last 12 hours and are never refreshed; the admin app
does not talk to the provider again after login. Logout is local and does not end
the provider session.

`OIDC_ALLOWED_GROUPS` adds an Admin-side group allowlist. When it is unset, the
identity provider controls which accounts may use the client, and every accepted
account becomes an administrator. The Admin app logs this delegated access model
at startup.

Keeping `ADMIN_TOKEN` alongside OIDC gives you a break-glass login for when the
provider is unreachable. Omitting it makes single sign-on the only way in and
removes the token form from the login page.

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
ADMIN_INSTANCE_NAME=fg-1-eu \
ADMIN_TOKEN=admin-token \
bun run dev
```

Open `http://127.0.0.1:3000` and sign in with `ADMIN_TOKEN`.

## Production deployment

The Admin app is source-shipped. Build `admin/Dockerfile` from a pinned Filegate
commit, or deploy the SSR app from `admin/` after
`bun install --frozen-lockfile` and `bun run build`.

Terminate TLS at the reverse proxy and keep the Admin app on an operator-only
network. It is not a read-only dashboard: any authenticated Admin user has the
Filegate bearer token's full effective authority through the server.

One Admin replica works with the in-memory login limiter. Multiple replicas
must share the same `ADMIN_SESSION_SECRET` and set `REDIS_URL` so login limits
are shared. Filegate itself remains single-node; scaling the UI does not make
the storage daemon active-active.

## Browser transfer behavior

| Operation | Data path | Meaning |
|---|---|---|
| Upload | Browser to Filegate | Admin app creates sessions; browser sends file bytes to scoped Filegate URLs. |
| Download | Browser from Filegate | Admin app mints a scoped URL and redirects browser to it. |
| Metadata mutation | Browser to admin to Filegate | Admin app submits REST calls with the server-side bearer token. |
