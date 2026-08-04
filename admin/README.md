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
ADMIN_INSTANCE_NAME=fg-1-eu \
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
| `ADMIN_INSTANCE_NAME` | no | Instance name shown in the admin shell, login page and browser title. Defaults to `Filegate Admin`. |
| `ADMIN_TOKEN` | see note | Admin login token. Must differ from `FILEGATE_TOKEN`. Required unless OIDC is configured, where it stays useful as a break-glass login. |
| `ADMIN_SESSION_SECRET` | no | Session signing secret. Generated at boot when unset, which means sessions survive neither a restart nor a second replica. Set it in production. |
| `PORT` | no | Listen port, default `3000`. |
| `ADMIN_TRUST_PROXY` | no | Set when a reverse proxy sits in front, so `X-Forwarded-For` is used for rate limiting instead of the socket address. |
| `ADMIN_COOKIE_SECURE` | no | `auto` (default), `true` or `false`. Auto marks the session cookie `Secure` unless the request host is localhost. |
| `REDIS_URL` | no | Enables shared rate limiting across replicas. In-memory otherwise. |

`ADMIN_TOKEN` no longer falls back to `FILEGATE_TOKEN`, and startup fails when
the two are equal: sharing them means guessing the admin login hands out the
Filegate master credential. `ADMIN_SESSION_SECRET` must likewise differ from both.

## Single sign-on (OIDC)

Setting these four together enables OIDC; leave them unset for token-only login.

| Variable | Required | Meaning |
|---|---:|---|
| `OIDC_ISSUER` | yes | Issuer URL. Discovery reads `<issuer>/.well-known/openid-configuration`. Must be https outside localhost. |
| `OIDC_CLIENT_ID` | yes | Client id. |
| `OIDC_CLIENT_SECRET` | yes | Client secret; stays server-side. |
| `OIDC_REDIRECT_URL` | yes | Must match the client's redirect URI, ending in `/auth/callback`. |
| `OIDC_SCOPES` | no | Default `openid profile email`. `openid` is added if missing. |
| `OIDC_GROUPS_CLAIM` | no | Claim holding group membership, default `groups`. |
| `OIDC_ALLOWED_GROUPS` | no | Comma-separated allowlist. **When unset, anyone your provider lets through this client becomes an admin.** |

`OIDC_ALLOWED_GROUPS` is deliberately optional: providers such as Authentik bind
a group policy to the application itself, so a second allowlist here would be
duplicate bookkeeping. Leaving it unset delegates access control to the provider
and logs a warning at startup saying so.

Authorization code flow with PKCE. The ID token is verified against the
provider's JWKS, and issuer, audience and nonce are all checked. Sessions last 12
hours and are not refreshed; there is no call to the provider after login.

When both are configured the login page offers both, and `ADMIN_TOKEN` remains a
break-glass path. Drop `ADMIN_TOKEN` to make single sign-on the only way in; the
token form then disappears.

Logout is local to the admin app and does not end the session at the provider.

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
