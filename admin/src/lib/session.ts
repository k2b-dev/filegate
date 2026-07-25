import { createHmac, timingSafeEqual } from "node:crypto";
import { env } from "./env";

export const sessionCookieName = "filegate_admin";
export const sessionTtlSeconds = 12 * 60 * 60;

/** How the person at the keyboard proved who they are. */
export type SessionKind = "token" | "oidc";

export type AdminSession = {
  /** Stable subject. For token logins there is only one, for OIDC it is the IdP subject. */
  sub: string;
  /** Human-readable label used in the UI and, later, in Filegate audit entries. */
  label: string;
  kind: SessionKind;
  issuedAt: number;
  expiresAt: number;
};

declare module "hono" {
  interface ContextVariableMap {
    session: AdminSession;
  }
}

type SessionPayload = {
  v: 1;
  sub: string;
  label: string;
  kind: SessionKind;
  iat: number;
  exp: number;
};

function sign(payload: string): string {
  return createHmac("sha256", env().sessionSecret).update(payload).digest("base64url");
}

function equal(a: string, b: string): boolean {
  const ab = Buffer.from(a);
  const bb = Buffer.from(b);
  return ab.length === bb.length && timingSafeEqual(ab, bb);
}

/**
 * Issue a stateless signed session. The payload travels in the cookie so there
 * is no session store to run; the signature is what makes it trustworthy, and
 * expiresAt is verified server-side on every request rather than being left to
 * the browser's cookie expiry.
 */
export function issueSession(input: { sub: string; label: string; kind: SessionKind; now?: number }): {
  value: string;
  session: AdminSession;
} {
  const now = input.now ?? Date.now();
  const payload: SessionPayload = {
    v: 1,
    sub: input.sub,
    label: input.label,
    kind: input.kind,
    iat: Math.floor(now / 1000),
    exp: Math.floor(now / 1000) + sessionTtlSeconds,
  };
  const encoded = Buffer.from(JSON.stringify(payload), "utf8").toString("base64url");
  return {
    value: `${encoded}.${sign(encoded)}`,
    session: toSession(payload),
  };
}

/** Verify signature, shape and expiry. Returns null for anything untrustworthy. */
export function readSession(value: string | undefined, now = Date.now()): AdminSession | null {
  if (!value) return null;
  const cut = value.lastIndexOf(".");
  if (cut <= 0) return null;

  const encoded = value.slice(0, cut);
  if (!equal(value.slice(cut + 1), sign(encoded))) return null;

  let payload: SessionPayload;
  try {
    payload = JSON.parse(Buffer.from(encoded, "base64url").toString("utf8"));
  } catch {
    return null;
  }

  if (payload?.v !== 1) return null;
  if (typeof payload.sub !== "string" || !payload.sub) return null;
  if (payload.kind !== "token" && payload.kind !== "oidc") return null;
  if (typeof payload.exp !== "number" || payload.exp * 1000 <= now) return null;

  return toSession(payload);
}

function toSession(payload: SessionPayload): AdminSession {
  return {
    sub: payload.sub,
    label: typeof payload.label === "string" && payload.label ? payload.label : payload.sub,
    kind: payload.kind,
    issuedAt: payload.iat * 1000,
    expiresAt: payload.exp * 1000,
  };
}
