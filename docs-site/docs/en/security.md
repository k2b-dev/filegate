---
title: Security model
navTitle: Security model
section: Operate
order: 130
description: Understand Filegate authentication boundaries, the beta scope, direct URL tokens, CORS, trusted proxies, and secret handling.
tags: [security, auth, cors]
---

# Security model

This page is for operators and developers who need to understand Filegate authentication boundaries and browser-safe transfer patterns.

## Beta scope

Filegate is in beta, and the authorization model is deliberately small. Read this before deciding where to put it.

| Property | State |
|---|---|
| Tenancy | Single tenant. Every mount is visible to every credential that can reach the REST API. |
| REST authorization | One bearer token, all or nothing. There are no per-path, per-mount, or read-only REST credentials. |
| S3 authorization | Multiple access keys, scoped to buckets and rate limited. This is the only place where credentials differ in what they may do. |
| Admin authorization | Any account that passes the admin login is a full administrator. There are no admin roles. |
| Network | Assumes a trusted network. Filegate expects to sit behind a reverse proxy or inside a private network, not on the public internet with the REST port exposed. |
| Audit | Activity records name the credential that acted. They are a ring buffer in memory, not a durable audit log. |

The practical consequence: anyone holding the bearer token can read and delete every file on every mount, and can change the service's own configuration. Give it to services, not to people, and use S3 keys when you need to hand out something narrower.

## Authentication surfaces

| Surface | Scope | Credential |
|---|---:|---|
| REST API | `/v1/*` routes | Bearer token from `auth.bearer_token`, or the one generated at first start. |
| Config API | `/v1/config*` routes | Same bearer token. Can change runtime configuration. |
| Health | `/health` | No credential. |
| Metrics | Configured metrics path | `metrics.token`, falling back to the REST bearer token. |
| S3 API | S3 listener | SigV4 access key and secret key. |
| Direct upload URL | One upload target | Scoped URL token. |
| Direct download URL | One resolved node | Scoped URL token. |
| Direct upload session | One upload session | Scoped session token. |

A REST bearer token always exists. When `auth.bearer_token` is unset, the first start generates one, stores it in the runtime config store, and prints it once. There is no unauthenticated REST deployment.

The config routes deserve separate thought. An actor who can `PATCH /v1/config` can widen CORS, disable access logs, or raise upload limits. They carry the authority of the bearer token, so the token's blast radius includes the service's own settings.

## Actor logging

Filegate logs the authenticated credential kind as the actor. `X-Filegate-Actor` can add a delegated actor label for applications that want user-level attribution.

| Field | Scope | Meaning |
|---|---:|---|
| `actor.kind` | Activity event | `bearer_token`, `s3_key`, `signed_url`, or `system`. |
| `actor.id` | Activity event | Stable identifier for the authenticated credential. |
| `actor.label` | Activity event | Optional credential label. |
| `actor.delegatedActor` | Activity event | Optional label from `X-Filegate-Actor`. |

`X-Filegate-Actor` is not authorization. Treat it as log metadata supplied by a trusted application server.

## Browser uploads

Do not expose the Filegate bearer token to browsers. Use an application server to create direct upload sessions or direct one-shot URLs.

```txt
browser -> app server: request upload permission
app server -> Filegate: create scoped direct URL or session
browser -> Filegate: upload bytes with scoped token
```

## CORS

CORS is disabled when `server.cors.allowed_origins` is empty. Prefer configuring CORS at the reverse proxy. If Filegate must answer browser CORS directly, configure explicit origins.

```yaml
server:
  cors:
    allowed_origins:
      - "https://app.example.com"
    exposed_headers:
      - "X-Node-Id"
      - "X-Created-Id"
```

Wildcard origin with `allow_credentials: true` is rejected.

CORS is a runtime setting, so it can be changed through the admin UI and applies to the next request without a restart.

## Trusted proxies

`server.trusted_proxies` controls whether `X-Forwarded-For` and `X-Real-Ip` are honored for logged client addresses.

| Setting | Scope | Meaning |
|---|---:|---|
| Empty list | Service | Ignore forwarded client IP headers. Default. |
| IP or CIDR entry | Proxy peer | Honor forwarded headers only from matching peers. |

Behind Traefik, Caddy, or nginx, list the proxy address or CIDR. Do not trust forwarded headers from direct clients.

## Secret handling

| Secret | Storage scope | Handling |
|---|---:|---|
| REST bearer token | Config, environment, or runtime config store | Keep server-side. Generated and printed once when unconfigured. |
| S3 secret keys | Runtime config store, seeded from config | Shown once at creation or rotation and never again. |
| Metrics token | Config or environment | Use for scraper-only access. |
| Admin app session secret | Admin process | Keep server-side. |

Secrets are never returned by the config API or printed by `fg config show`; those surfaces report only whether a value is configured.

The runtime config store at `storage.runtime_config_path` holds the generated bearer token and every S3 secret. Protect and back it up like a credential store, because that is what it is.
