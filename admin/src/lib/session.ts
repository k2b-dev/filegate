import { seal, unseal } from "./signed";

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
};

/**
 * Issue a stateless signed session. The payload travels in the cookie so there
 * is no session store to run; expiry is enforced server-side on every request.
 */
export function issueSession(input: { sub: string; label: string; kind: SessionKind; now?: number }): {
  value: string;
  session: AdminSession;
} {
  const payload: SessionPayload = { v: 1, sub: input.sub, label: input.label, kind: input.kind };
  const value = seal(payload, sessionTtlSeconds, input.now);
  const session = readSession(value, input.now);
  if (!session) throw new Error("issued session failed verification");
  return { value, session };
}

/** Verify signature, shape and expiry. Returns null for anything untrustworthy. */
export function readSession(value: string | undefined, now = Date.now()): AdminSession | null {
  const payload = unseal<SessionPayload>(value, now);
  if (!payload) return null;

  if (payload.v !== 1) return null;
  if (typeof payload.sub !== "string" || !payload.sub) return null;
  if (payload.kind !== "token" && payload.kind !== "oidc") return null;

  return {
    sub: payload.sub,
    label: typeof payload.label === "string" && payload.label ? payload.label : payload.sub,
    kind: payload.kind,
    expiresAt: payload.exp * 1000,
  };
}
