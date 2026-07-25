# Filegate Admin

Standalone SSR admin app for Filegate.

The app keeps the Filegate bearer token server-side and talks to Filegate through
the TypeScript client. The browser only receives an admin session cookie.

## Run locally

```bash
cd admin
bun install
FILEGATE_URL=http://127.0.0.1:18080 \
FILEGATE_TOKEN=dev-token \
ADMIN_TOKEN=dev-admin \
ADMIN_SESSION_SECRET=dev-session-secret \
bun run dev
```

Open `http://127.0.0.1:3000` and sign in with `ADMIN_TOKEN`.

## Configuration

| Variable | Required | Meaning |
|---|---:|---|
| `FILEGATE_URL` | yes | REST API base URL, reachable from the admin server. |
| `FILEGATE_TOKEN` | yes | Filegate bearer token, kept server-side. |
| `ADMIN_TOKEN` | yes | Admin login token. Must differ from `FILEGATE_TOKEN`. |
| `ADMIN_SESSION_SECRET` | no | Session signing secret. Generated at boot when unset, which means sessions survive neither a restart nor a second replica. Set it in production. |
| `PORT` | no | Listen port, default `3000`. |
| `ADMIN_TRUST_PROXY` | no | Set when a reverse proxy sits in front, so `X-Forwarded-For` is used for rate limiting instead of the socket address. |
| `ADMIN_COOKIE_SECURE` | no | `auto` (default), `true` or `false`. Auto marks the session cookie `Secure` unless the request host is localhost. |
| `REDIS_URL` | no | Enables shared rate limiting across replicas. In-memory otherwise. |

`ADMIN_TOKEN` no longer falls back to `FILEGATE_TOKEN`, and startup fails when
the two are equal: sharing them means guessing the admin login hands out the
Filegate master credential. `ADMIN_SESSION_SECRET` must likewise differ from both.

## Sessions and login

Sign-in issues a stateless signed session cookie carrying subject, label and
expiry, verified server-side on every request. There is no session store to run.

`POST /login` is rate limited to 10 attempts per 5 minutes per client. The limiter
is in-memory by default; setting `REDIS_URL` switches it to a Redis-backed one
that is shared across replicas. Note that `REDIS_URL` must be present in the
process environment at launch, since the Redis connection is resolved from it at
startup and not re-read later.

## Uploads

The files page creates upload sessions through the admin server, then uploads
segments directly from the browser to Filegate with scoped direct session tokens.
This keeps large uploads out of the admin server request path while still hiding
the Filegate bearer token from the browser.

Downloads use the same shape: the admin server mints a scoped direct download
URL, then redirects the browser to Filegate.

When Filegate is on a different browser origin, configure Filegate CORS for the
admin app origin. The default Filegate CORS headers include
`Filegate-Upload-Session` and `X-Segment-Checksum`.
