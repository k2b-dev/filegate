import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import type { Hono } from "hono";
import { encodeClaims, startFakeIdp, type FakeClaims, type FakeIdp } from "./fake-idp";

const adminToken = "admin-token";
let idp: FakeIdp;
let app: Hono;
let unseal: typeof import("../src/lib/signed").unseal;
let oidcStateCookieName: string;

function get(path: string, cookie?: string): Promise<Response> {
  return app.fetch(
    new Request(`http://localhost${path}`, {
      headers: { host: "localhost", ...(cookie ? { cookie } : {}) },
      redirect: "manual",
    }),
  );
}

function cookieValue(res: Response, name: string): string | undefined {
  for (const entry of res.headers.getSetCookie()) {
    const [pair] = entry.split(";");
    const [key, ...rest] = (pair ?? "").split("=");
    if (key === name) return rest.join("=");
  }
  return undefined;
}

/** Runs the redirect to the provider and returns the flow state the app sealed. */
async function beginFlow() {
  const res = await get("/auth/login");
  const raw = cookieValue(res, oidcStateCookieName);
  const flow = unseal<{ state: string; nonce: string; verifier: string }>(raw);
  if (!flow) throw new Error("no OIDC flow state cookie was set");
  return { res, flow, cookie: `${oidcStateCookieName}=${raw}` };
}

/** Completes a flow with claims of our choosing, defaulting to a valid login. */
async function callback(overrides: FakeClaims & { state?: string } = {}) {
  const { flow, cookie } = await beginFlow();
  const { state, ...claims } = overrides;
  const code = encodeClaims({ nonce: flow.nonce, email: "ada@example.com", groups: ["filegate-admins"], ...claims });
  return get(`/auth/callback?code=${code}&state=${encodeURIComponent(state ?? flow.state)}`, cookie);
}

function reasonOf(res: Response): string {
  return new URL(res.headers.get("location") ?? "", "http://localhost").searchParams.get("reason") ?? "";
}

beforeAll(async () => {
  idp = await startFakeIdp();

  Bun.env.FILEGATE_URL = "http://127.0.0.1:65535";
  Bun.env.FILEGATE_TOKEN = "filegate-token";
  Bun.env.ADMIN_TOKEN = adminToken;
  Bun.env.ADMIN_SESSION_SECRET = "session-secret";
  Bun.env.OIDC_ISSUER = idp.issuer;
  Bun.env.OIDC_CLIENT_ID = idp.clientId;
  Bun.env.OIDC_CLIENT_SECRET = idp.clientSecret;
  Bun.env.OIDC_REDIRECT_URL = "http://localhost/auth/callback";
  Bun.env.OIDC_ALLOWED_GROUPS = "filegate-admins";
  delete Bun.env.REDIS_URL;

  app = (await import("../src/app")).app;
  const signed = await import("../src/lib/signed");
  unseal = signed.unseal;
  oidcStateCookieName = (await import("../src/lib/oidc")).oidcStateCookieName;
});

afterAll(() => idp.stop());

describe("authorization request", () => {
  test("redirects to the provider with PKCE and a nonce", async () => {
    const { res, flow } = await beginFlow();
    const target = new URL(res.headers.get("location") ?? "");

    expect(res.status).toBe(303);
    expect(target.origin).toBe(idp.issuer);
    expect(target.pathname).toBe("/authorize");
    expect(target.searchParams.get("response_type")).toBe("code");
    expect(target.searchParams.get("client_id")).toBe(idp.clientId);
    expect(target.searchParams.get("redirect_uri")).toBe("http://localhost/auth/callback");
    expect(target.searchParams.get("scope")).toContain("openid");
    expect(target.searchParams.get("code_challenge_method")).toBe("S256");
    expect(target.searchParams.get("code_challenge")).toBeTruthy();
    // The challenge is derived, never the raw verifier.
    expect(target.searchParams.get("code_challenge")).not.toBe(flow.verifier);
    expect(target.searchParams.get("state")).toBe(flow.state);
    expect(target.searchParams.get("nonce")).toBe(flow.nonce);
  });

  test("the flow cookie is Lax so the provider's redirect can carry it", async () => {
    const res = await get("/auth/login");
    const entry = res.headers.getSetCookie().find((value) => value.startsWith(`${oidcStateCookieName}=`)) ?? "";

    // Strict would drop the cookie on the cross-site callback navigation and
    // every single sign-on attempt would fail with a state error.
    expect(entry).toContain("SameSite=Lax");
    expect(entry).toContain("HttpOnly");
  });
});

describe("callback", () => {
  test("issues an OIDC session and clears the flow cookie", async () => {
    const res = await callback();

    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe("/");

    const session = cookieValue(res, "filegate_admin");
    expect(session).toBeTruthy();

    const payload = unseal<{ sub: string; label: string; kind: string }>(session);
    expect(payload?.kind).toBe("oidc");
    expect(payload?.sub).toBe("user-1");
    // email is preferred over sub as the human-readable label.
    expect(payload?.label).toBe("ada@example.com");

    const cleared = res.headers.getSetCookie().find((value) => value.startsWith(`${oidcStateCookieName}=`));
    expect(cleared).toMatch(/Max-Age=0|Expires=Thu, 01 Jan 1970/);
  });

  test("the issued session opens authenticated pages", async () => {
    const login = await callback();
    const res = await get("/", `filegate_admin=${cookieValue(login, "filegate_admin")}`);

    expect(res.status).toBe(200);
  });

  test("uses basic auth for the code exchange and sends the PKCE verifier", async () => {
    idp.tokenRequests.length = 0;
    await callback();

    const request = idp.tokenRequests.at(-1);
    expect(request?.authorization).toStartWith("Basic ");
    expect(request?.body.grant_type).toBe("authorization_code");
    expect(request?.body.code_verifier).toBeTruthy();
    // Credentials must not also travel in the body when basic auth is used.
    expect(request?.body.client_secret).toBeUndefined();
  });

  test("falls back to a display name, then the subject, when email is absent", async () => {
    const withName = await callback({ email: undefined, name: "Ada Lovelace" });
    expect(unseal<{ label: string }>(cookieValue(withName, "filegate_admin"))?.label).toBe("Ada Lovelace");

    const bare = await callback({ email: undefined, name: undefined, sub: "opaque-subject" });
    expect(unseal<{ label: string }>(cookieValue(bare, "filegate_admin"))?.label).toBe("opaque-subject");
  });
});

describe("callback rejections", () => {
  test("rejects a mismatched state", async () => {
    const res = await callback({ state: "not-the-state" });

    expect(reasonOf(res)).toBe("state");
    expect(cookieValue(res, "filegate_admin")).toBeUndefined();
  });

  test("rejects a callback with no flow cookie", async () => {
    const res = await get("/auth/callback?code=abc&state=abc");

    expect(reasonOf(res)).toBe("state");
    expect(cookieValue(res, "filegate_admin")).toBeUndefined();
  });

  test("rejects a callback with no authorization code", async () => {
    const { flow, cookie } = await beginFlow();
    const res = await get(`/auth/callback?state=${flow.state}`, cookie);

    expect(reasonOf(res)).toBe("state");
  });

  test("surfaces a provider-reported error", async () => {
    const { flow, cookie } = await beginFlow();
    const res = await get(`/auth/callback?error=access_denied&state=${flow.state}`, cookie);

    expect(reasonOf(res)).toBe("provider");
  });

  test("rejects a replayed nonce from another attempt", async () => {
    const res = await callback({ nonce: "nonce-from-a-different-login" });

    expect(reasonOf(res)).toBe("nonce");
    expect(cookieValue(res, "filegate_admin")).toBeUndefined();
  });

  test("rejects a token signed by an unknown key", async () => {
    const res = await callback({ signWithUnknownKey: true });

    expect(reasonOf(res)).toBe("token");
    expect(cookieValue(res, "filegate_admin")).toBeUndefined();
  });

  test("rejects a token issued for a different audience", async () => {
    const res = await callback({ audience: "some-other-client" });

    expect(reasonOf(res)).toBe("token");
  });

  test("rejects a token response without an id_token", async () => {
    const res = await callback({ omitIdToken: true });

    expect(reasonOf(res)).toBe("token");
  });
});

describe("group allowlist", () => {
  test("admits a member of an allowed group", async () => {
    const res = await callback({ groups: ["users", "filegate-admins"] });

    expect(res.headers.get("location")).toBe("/");
  });

  test("rejects an account outside the allowed groups", async () => {
    const res = await callback({ groups: ["users"] });

    expect(reasonOf(res)).toBe("group");
    expect(cookieValue(res, "filegate_admin")).toBeUndefined();
  });

  test("rejects an account with no groups at all", async () => {
    const res = await callback({ groups: undefined });

    expect(reasonOf(res)).toBe("group");
  });

  test("accepts a delimited group string, as some providers send", async () => {
    const res = await callback({ groups: "users filegate-admins" });

    expect(res.headers.get("location")).toBe("/");
  });
});

describe("coexistence with the token login", () => {
  test("both sign-in options are offered when both are configured", async () => {
    const body = await (await get("/login")).text();

    expect(body).toContain("/auth/login");
    expect(body).toContain('name="token"');
  });

  test("the token login still works", async () => {
    const res = await app.fetch(
      new Request("http://localhost/login", {
        method: "POST",
        body: new URLSearchParams({ token: adminToken }),
        headers: { "content-type": "application/x-www-form-urlencoded", host: "localhost" },
        redirect: "manual",
      }),
    );

    expect(res.headers.get("location")).toBe("/");
    expect(unseal<{ kind: string }>(cookieValue(res, "filegate_admin"))?.kind).toBe("token");
  });
});
