import { timingSafeEqual } from "node:crypto";
import type { Context, MiddlewareHandler } from "hono";
import { deleteCookie, getCookie, setCookie } from "hono/cookie";
import { env } from "./env";
import { beginLogin, completeLogin, OidcError, oidcStateCookieName, oidcStateTtlSeconds, type OidcFlowState } from "./oidc";
import { recordLoginAttempt } from "./ratelimit";
import { clientId, useSecureCookie } from "./request";
import { issueSession, readSession, sessionCookieName, sessionTtlSeconds, type AdminSession } from "./session";
import { seal, unseal } from "./signed";

/** Subject used for the shared-token login; OIDC logins carry the IdP subject. */
const tokenSubject = "local-admin";
const tokenLabel = "Local admin";

function equal(a: string, b: string): boolean {
  const ab = Buffer.from(a);
  const bb = Buffer.from(b);
  return ab.length === bb.length && timingSafeEqual(ab, bb);
}

/** Which sign-in options to offer on the login page. */
export function authMethods(): { token: boolean; oidc: boolean } {
  const cfg = env();
  return { token: !!cfg.adminToken, oidc: !!cfg.oidc };
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
  const adminToken = env().adminToken;
  if (!adminToken) return c.redirect("/login", 303);

  // Count the attempt before checking the token, so a wrong guess costs budget.
  const attempt = await recordLoginAttempt(clientId(c));
  if (attempt.limited) {
    c.header("Retry-After", String(attempt.retryAfterSeconds));
    return c.redirect(`/login?error=throttled&retry=${attempt.retryAfterSeconds}`, 303);
  }

  const body = await c.req.parseBody();
  const token = String(body.token || "");
  if (!equal(token, adminToken)) {
    return c.redirect("/login?error=invalid", 303);
  }

  establish(c, { sub: tokenSubject, label: tokenLabel, kind: "token" });
  return c.redirect("/", 303);
}

export function logout(c: Context): Response {
  deleteCookie(c, sessionCookieName, { path: "/" });
  return c.redirect("/login", 303);
}

function oidcFailure(c: Context, err: unknown): Response {
  const reason = err instanceof OidcError ? err.reason : "unknown";
  console.error(`[filegate-admin] OIDC login failed (${reason}):`, err instanceof Error ? err.message : err);
  return c.redirect(`/login?error=oidc&reason=${encodeURIComponent(reason)}`, 303);
}

export async function oidcBegin(c: Context): Promise<Response> {
  if (!env().oidc) return c.redirect("/login", 303);

  try {
    const { redirectTo, flow } = await beginLogin();
    setCookie(c, oidcStateCookieName, seal(flow, oidcStateTtlSeconds), {
      httpOnly: true,
      // Lax, not Strict: the callback arrives as a top-level navigation from the
      // identity provider, and a Strict cookie would not be sent with it.
      sameSite: "Lax",
      secure: useSecureCookie(c),
      path: "/",
      maxAge: oidcStateTtlSeconds,
    });
    return c.redirect(redirectTo, 303);
  } catch (err) {
    return oidcFailure(c, err);
  }
}

export async function oidcCallback(c: Context): Promise<Response> {
  if (!env().oidc) return c.redirect("/login", 303);

  const clearState = () => deleteCookie(c, oidcStateCookieName, { path: "/" });

  try {
    // The provider reports user-facing failures such as a denied consent here.
    const providerError = c.req.query("error");
    if (providerError) throw new OidcError("provider", `identity provider returned ${providerError}`);

    const flow = unseal<OidcFlowState>(getCookie(c, oidcStateCookieName));
    if (!flow) throw new OidcError("state", "OIDC login state is missing or expired; start the login again");

    const code = c.req.query("code");
    if (!code) throw new OidcError("state", "callback did not include an authorization code");

    const identity = await completeLogin({ code, state: c.req.query("state") ?? "", flow });

    clearState();
    establish(c, { sub: identity.sub, label: identity.label, kind: "oidc" });
    console.log(`[filegate-admin] OIDC login: ${identity.label} (${identity.sub})`);
    return c.redirect("/", 303);
  } catch (err) {
    clearState();
    return oidcFailure(c, err);
  }
}
