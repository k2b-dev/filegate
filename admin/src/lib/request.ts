import type { Context } from "hono";
import { env } from "./env";

type BunServerEnv = { server?: { requestIP(req: Request): { address: string } | null } };

/**
 * Rate-limit bucket key for the caller.
 *
 * X-Forwarded-For is only consulted when ADMIN_TRUST_PROXY is set, because any
 * client can send that header: trusting it unconditionally would let an
 * attacker pick a fresh bucket per request and defeat the limit entirely.
 * Without a trusted proxy we use the socket address instead.
 */
export function clientId(c: Context): string {
  if (env().trustProxy) {
    const forwarded = c.req.header("x-forwarded-for")?.split(",")[0]?.trim();
    if (forwarded) return forwarded;
    const real = c.req.header("x-real-ip")?.trim();
    if (real) return real;
  }

  const address = (c.env as BunServerEnv | undefined)?.server?.requestIP(c.req.raw)?.address;
  // Callers we cannot identify share one bucket. That is deliberately strict:
  // it throttles rather than exempts them.
  return address || "unknown";
}

const localHosts = new Set(["localhost", "127.0.0.1", "::1", "[::1]"]);

function isLocalRequest(c: Context): boolean {
  const host = c.req.header("host")?.split(":")[0]?.trim().toLowerCase();
  return !!host && localHosts.has(host);
}

/**
 * Whether the session cookie should carry the Secure flag.
 *
 * The previous implementation derived this from the request URL's protocol,
 * which is http inside the container behind a TLS-terminating ingress — so the
 * cookie shipped without Secure exactly where it mattered most. Now the default
 * is secure, with localhost as the only automatic exception so plain-http local
 * development still works, and an explicit override for anything unusual.
 */
export function useSecureCookie(c: Context): boolean {
  switch (env().cookieSecure) {
    case "always":
      return true;
    case "never":
      return false;
    default:
      return !isLocalRequest(c);
  }
}
