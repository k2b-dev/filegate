import { timingSafeEqual } from "node:crypto";
import type { Context, MiddlewareHandler } from "hono";
import { deleteCookie, getCookie, setCookie } from "hono/cookie";
import { env } from "./env";
import { recordLoginAttempt } from "./ratelimit";
import { clientId, useSecureCookie } from "./request";
import { issueSession, readSession, sessionCookieName, sessionTtlSeconds, type AdminSession } from "./session";

/** Subject used for the shared-token login; OIDC logins carry the IdP subject. */
const tokenSubject = "local-admin";
const tokenLabel = "Local admin";

function equal(a: string, b: string): boolean {
  const ab = Buffer.from(a);
  const bb = Buffer.from(b);
  return ab.length === bb.length && timingSafeEqual(ab, bb);
}

/** The verified session for this request, or null when unauthenticated. */
export function currentSession(c: Context): AdminSession | null {
  const existing = c.get("session");
  if (existing) return existing;

  const session = readSession(getCookie(c, sessionCookieName));
  if (session) c.set("session", session);
  return session;
}

export function requireAuth(): MiddlewareHandler {
  return async (c, next) => {
    if (currentSession(c)) return next();
    // Clear a cookie that is present but no longer valid, so an expired session
    // does not keep bouncing off the login page with a stale cookie attached.
    if (getCookie(c, sessionCookieName)) deleteCookie(c, sessionCookieName, { path: "/" });
    return c.redirect("/login", 303);
  };
}

function establish(c: Context, input: { sub: string; label: string; kind: AdminSession["kind"] }): void {
  const { value } = issueSession(input);
  setCookie(c, sessionCookieName, value, {
    httpOnly: true,
    sameSite: "Strict",
    secure: useSecureCookie(c),
    path: "/",
    maxAge: sessionTtlSeconds,
  });
}

export async function login(c: Context): Promise<Response> {
  // Count the attempt before checking the token, so a wrong guess costs budget.
  const attempt = await recordLoginAttempt(clientId(c));
  if (attempt.limited) {
    c.header("Retry-After", String(attempt.retryAfterSeconds));
    return c.redirect(`/login?error=throttled&retry=${attempt.retryAfterSeconds}`, 303);
  }

  const body = await c.req.parseBody();
  const token = String(body.token || "");
  if (!equal(token, env().adminToken)) {
    return c.redirect("/login?error=invalid", 303);
  }

  establish(c, { sub: tokenSubject, label: tokenLabel, kind: "token" });
  return c.redirect("/", 303);
}

export function logout(c: Context): Response {
  deleteCookie(c, sessionCookieName, { path: "/" });
  return c.redirect("/login", 303);
}
