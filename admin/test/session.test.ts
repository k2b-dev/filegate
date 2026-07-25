import { beforeAll, describe, expect, test } from "bun:test";

let issueSession: typeof import("../src/lib/session").issueSession;
let readSession: typeof import("../src/lib/session").readSession;
let sessionTtlSeconds: number;

beforeAll(async () => {
  Bun.env.FILEGATE_URL = "http://127.0.0.1:65535";
  Bun.env.FILEGATE_TOKEN = "filegate-token";
  Bun.env.ADMIN_TOKEN = "admin-token";
  Bun.env.ADMIN_SESSION_SECRET = "session-secret";

  const mod = await import("../src/lib/session");
  issueSession = mod.issueSession;
  readSession = mod.readSession;
  sessionTtlSeconds = mod.sessionTtlSeconds;
});

describe("session tokens", () => {
  test("round-trips subject, label and kind", () => {
    const { value } = issueSession({ sub: "user-1", label: "Ada", kind: "oidc" });
    const session = readSession(value);

    expect(session).not.toBeNull();
    expect(session?.sub).toBe("user-1");
    expect(session?.label).toBe("Ada");
    expect(session?.kind).toBe("oidc");
  });

  test("two sessions for different subjects differ", () => {
    const a = issueSession({ sub: "user-1", label: "Ada", kind: "oidc" }).value;
    const b = issueSession({ sub: "user-2", label: "Grace", kind: "oidc" }).value;

    // The previous implementation produced one constant value for every login.
    expect(a).not.toBe(b);
  });

  test("rejects a tampered payload", () => {
    const { value } = issueSession({ sub: "user-1", label: "Ada", kind: "token" });
    const [payload, signature] = value.split(".");
    const forged = Buffer.from(JSON.stringify({ v: 1, sub: "root", label: "root", kind: "token", iat: 0, exp: 9e9 }), "utf8").toString("base64url");

    expect(readSession(`${forged}.${signature}`)).toBeNull();
    expect(payload).toBeTruthy();
  });

  test("rejects a tampered signature", () => {
    const { value } = issueSession({ sub: "user-1", label: "Ada", kind: "token" });
    const cut = value.lastIndexOf(".");
    const flipped = value.slice(cut + 1, cut + 2) === "a" ? "b" : "a";

    expect(readSession(`${value.slice(0, cut + 1)}${flipped}${value.slice(cut + 2)}`)).toBeNull();
  });

  test("rejects malformed values", () => {
    expect(readSession(undefined)).toBeNull();
    expect(readSession("")).toBeNull();
    expect(readSession("no-separator")).toBeNull();
    expect(readSession(".onlysig")).toBeNull();
  });

  test("enforces expiry server-side", () => {
    const now = Date.now();
    const { value } = issueSession({ sub: "user-1", label: "Ada", kind: "token", now });

    expect(readSession(value, now + (sessionTtlSeconds - 5) * 1000)).not.toBeNull();
    expect(readSession(value, now + (sessionTtlSeconds + 5) * 1000)).toBeNull();
  });
});
