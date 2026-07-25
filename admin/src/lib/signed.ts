import { createHmac, timingSafeEqual } from "node:crypto";
import { env } from "./env";

/**
 * Signed, self-contained cookie values.
 *
 * Both the admin session and the short-lived OIDC flow state travel in cookies
 * rather than in server memory, so nothing has to be shared between replicas and
 * there is no store to expire. The signature is what makes them trustworthy and
 * `exp` is always checked here, never left to the browser's cookie expiry.
 */

type Envelope = { exp: number };

function signature(payload: string): string {
  return createHmac("sha256", env().sessionSecret).update(payload).digest("base64url");
}

function equal(a: string, b: string): boolean {
  const ab = Buffer.from(a);
  const bb = Buffer.from(b);
  return ab.length === bb.length && timingSafeEqual(ab, bb);
}

/** Wrap a payload with an expiry and sign it. */
export function seal<T extends object>(payload: T, ttlSeconds: number, now = Date.now()): string {
  const body: T & Envelope = { ...payload, exp: Math.floor(now / 1000) + ttlSeconds };
  const encoded = Buffer.from(JSON.stringify(body), "utf8").toString("base64url");
  return `${encoded}.${signature(encoded)}`;
}

/** Verify signature and expiry. Returns null for anything untrustworthy. */
export function unseal<T>(value: string | undefined, now = Date.now()): (T & Envelope) | null {
  if (!value) return null;

  const cut = value.lastIndexOf(".");
  if (cut <= 0) return null;

  const encoded = value.slice(0, cut);
  if (!equal(value.slice(cut + 1), signature(encoded))) return null;

  let payload: T & Envelope;
  try {
    payload = JSON.parse(Buffer.from(encoded, "base64url").toString("utf8"));
  } catch {
    return null;
  }

  if (typeof payload?.exp !== "number" || payload.exp * 1000 <= now) return null;
  return payload;
}
