import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import type { Hono } from "hono";
import { encodeClaims, startFakeIdp, type FakeIdp } from "./fake-idp";

/**
 * The other OIDC configuration shape: no group allowlist and no ADMIN_TOKEN.
 * Access control is delegated entirely to the identity provider, and single
 * sign-on is the only way in. Separate file because the resolved environment is
 * cached per process.
 */
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

async function loginAs(groups: string[] | undefined): Promise<Response> {
  const begin = await get("/auth/login");
  const raw = cookieValue(begin, oidcStateCookieName);
  const flow = unseal<{ state: string; nonce: string }>(raw);
  if (!flow) throw new Error("no OIDC flow state cookie was set");

  const code = encodeClaims({ nonce: flow.nonce, email: "someone@example.com", groups });
  return get(`/auth/callback?code=${code}&state=${encodeURIComponent(flow.state)}`, `${oidcStateCookieName}=${raw}`);
}

beforeAll(async () => {
  // Advertise only client_secret_post so the auth-method fallback is exercised.
  idp = await startFakeIdp({ authMethods: ["client_secret_post"] });

  Bun.env.FILEGATE_URL = "http://127.0.0.1:65535";
  Bun.env.FILEGATE_TOKEN = "filegate-token";
  Bun.env.ADMIN_SESSION_SECRET = "session-secret";
  Bun.env.OIDC_ISSUER = idp.issuer;
  Bun.env.OIDC_CLIENT_ID = idp.clientId;
  Bun.env.OIDC_CLIENT_SECRET = idp.clientSecret;
  Bun.env.OIDC_REDIRECT_URL = "http://localhost/auth/callback";
  delete Bun.env.ADMIN_TOKEN;
  delete Bun.env.OIDC_ALLOWED_GROUPS;
  delete Bun.env.REDIS_URL;

  app = (await import("../src/app")).app;
  unseal = (await import("../src/lib/signed")).unseal;
  oidcStateCookieName = (await import("../src/lib/oidc")).oidcStateCookieName;
});

afterAll(() => idp.stop());

describe("delegated access control", () => {
  test("admits any account when no allowlist is configured", async () => {
    // This is the documented behaviour: providers like Authentik restrict access
    // on the client itself, so a second allowlist here would be duplicate work.
    expect((await loginAs(["some-unrelated-group"])).headers.get("location")).toBe("/");
    expect((await loginAs(undefined)).headers.get("location")).toBe("/");
  });
});

describe("single sign-on only", () => {
  test("the login page offers no token form without ADMIN_TOKEN", async () => {
    const body = await (await get("/login")).text();

    expect(body).toContain("/auth/login");
    expect(body).not.toContain('name="token"');
  });

  test("posting to the token login is refused", async () => {
    const res = await app.fetch(
      new Request("http://localhost/login", {
        method: "POST",
        body: new URLSearchParams({ token: "anything" }),
        headers: { "content-type": "application/x-www-form-urlencoded", host: "localhost" },
        redirect: "manual",
      }),
    );

    expect(res.headers.get("location")).toBe("/login");
    expect(cookieValue(res, "filegate_admin")).toBeUndefined();
  });
});

describe("token endpoint auth method", () => {
  test("falls back to form credentials when basic auth is not advertised", async () => {
    idp.tokenRequests.length = 0;
    await loginAs(["any"]);

    const request = idp.tokenRequests.at(-1);
    expect(request?.authorization).toBeUndefined();
    expect(request?.body.client_id).toBe(idp.clientId);
    expect(request?.body.client_secret).toBe(idp.clientSecret);
  });
});
