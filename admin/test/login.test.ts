import { beforeAll, describe, expect, test } from "bun:test";
import type { Hono } from "hono";

const adminToken = "admin-token";
let app: Hono;

// Each test uses its own X-Forwarded-For so it gets its own rate-limit bucket.
// ADMIN_TRUST_PROXY makes that header authoritative, which is also the code path
// a deployment behind an ingress takes.
function request(path: string, init: RequestInit & { ip: string; cookie?: string }): Promise<Response> {
  const headers = new Headers(init.headers);
  headers.set("host", "localhost");
  headers.set("x-forwarded-for", init.ip);
  if (init.cookie) headers.set("cookie", init.cookie);
  return app.fetch(new Request(`http://localhost${path}`, { ...init, headers, redirect: "manual" }));
}

function loginBody(token: string): RequestInit {
  return {
    method: "POST",
    body: new URLSearchParams({ token }),
    headers: { "content-type": "application/x-www-form-urlencoded" },
  };
}

function sessionCookie(res: Response): string {
  const raw = res.headers.get("set-cookie") ?? "";
  return raw.split(";")[0] ?? "";
}

beforeAll(async () => {
  Bun.env.FILEGATE_URL = "http://127.0.0.1:65535";
  Bun.env.FILEGATE_TOKEN = "filegate-token";
  Bun.env.ADMIN_TOKEN = adminToken;
  Bun.env.ADMIN_SESSION_SECRET = "session-secret";
  Bun.env.ADMIN_TRUST_PROXY = "true";
  delete Bun.env.REDIS_URL;

  app = (await import("../src/app")).app;
});

describe("auth gate", () => {
  test("unauthenticated requests are redirected to the login page", async () => {
    const res = await request("/", { ip: "10.0.0.1" });

    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe("/login");
  });

  test("the login page and health endpoint stay public", async () => {
    expect((await request("/login", { ip: "10.0.0.2" })).status).toBe(200);
    expect((await request("/health", { ip: "10.0.0.2" })).status).toBe(200);
  });

  test("a forged cookie does not grant access", async () => {
    const res = await request("/", { ip: "10.0.0.3", cookie: "filegate_admin=forged.value" });

    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe("/login");
  });
});

describe("token login", () => {
  test("rejects a wrong token without issuing a cookie", async () => {
    const res = await request("/login", { ip: "10.0.1.1", ...loginBody("wrong") });

    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe("/login?error=invalid");
    expect(res.headers.get("set-cookie")).toBeNull();
  });

  test("accepts the admin token and issues a hardened cookie", async () => {
    const res = await request("/login", { ip: "10.0.1.2", ...loginBody(adminToken) });

    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe("/");

    const cookie = res.headers.get("set-cookie") ?? "";
    expect(cookie).toContain("filegate_admin=");
    expect(cookie).toContain("HttpOnly");
    expect(cookie).toContain("SameSite=Strict");
    // Host is localhost, so the automatic mode must not mark the cookie Secure
    // or the browser would refuse to send it back over plain http.
    expect(cookie).not.toContain("Secure");
  });

  test("the issued session opens authenticated pages", async () => {
    const login = await request("/login", { ip: "10.0.1.3", ...loginBody(adminToken) });
    const res = await request("/", { ip: "10.0.1.3", cookie: sessionCookie(login) });

    // Filegate is unreachable in this test, so the page renders its error
    // banner. What matters here is that the session passed the auth gate.
    expect(res.status).toBe(200);
  });

  test("logout clears the cookie", async () => {
    const login = await request("/login", { ip: "10.0.1.4", ...loginBody(adminToken) });
    const res = await request("/logout", { ip: "10.0.1.4", method: "POST", cookie: sessionCookie(login) });
    const cookie = res.headers.get("set-cookie") ?? "";

    expect(res.status).toBe(303);
    expect(cookie).toContain("filegate_admin=");
    expect(cookie).toMatch(/Max-Age=0|Expires=Thu, 01 Jan 1970/);
  });
});

describe("secure cookie mode", () => {
  test("a non-localhost host gets a Secure cookie even over plain http", async () => {
    const res = await app.fetch(
      new Request("http://admin.example.com/login", {
        method: "POST",
        body: new URLSearchParams({ token: adminToken }),
        headers: {
          "content-type": "application/x-www-form-urlencoded",
          "x-forwarded-for": "10.0.2.1",
          host: "admin.example.com",
        },
        redirect: "manual",
      }),
    );

    // This is the ingress case: the process sees http, the browser sees https.
    expect(res.headers.get("set-cookie")).toContain("Secure");
  });
});

describe("login rate limit", () => {
  test("throttles after the attempt budget is spent and reports Retry-After", async () => {
    const ip = "10.0.3.1";
    const statuses: string[] = [];

    for (let i = 0; i < 11; i++) {
      const res = await request("/login", { ip, ...loginBody("wrong") });
      statuses.push(res.headers.get("location") ?? "");
      if (i === 10) {
        expect(res.headers.get("retry-after")).toBeTruthy();
      }
    }

    expect(statuses.slice(0, 10).every((location) => location === "/login?error=invalid")).toBe(true);
    expect(statuses[10]).toContain("error=throttled");
  });

  test("a throttled client cannot log in even with the correct token", async () => {
    const ip = "10.0.3.2";
    for (let i = 0; i < 10; i++) await request("/login", { ip, ...loginBody("wrong") });

    const res = await request("/login", { ip, ...loginBody(adminToken) });
    expect(res.headers.get("location")).toContain("error=throttled");
    expect(res.headers.get("set-cookie")).toBeNull();
  });

  test("the limit is per client, not global", async () => {
    const noisy = "10.0.3.3";
    for (let i = 0; i < 11; i++) await request("/login", { ip: noisy, ...loginBody("wrong") });

    const res = await request("/login", { ip: "10.0.3.4", ...loginBody(adminToken) });
    expect(res.headers.get("location")).toBe("/");
  });
});
