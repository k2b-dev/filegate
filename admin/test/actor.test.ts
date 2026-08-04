import { beforeAll, describe, expect, test } from "bun:test";
import type { Hono } from "hono";

/**
 * Filegate attributes audit entries to the bearer token, so several admins
 * sharing one token are indistinguishable. These tests assert the admin sends
 * X-Filegate-Actor so the log names the human instead.
 */
let app: Hono;
let issueSession: typeof import("../src/lib/session").issueSession;
const seen: { actor?: string | null; path: string }[] = [];

beforeAll(async () => {
  // Stand-in Filegate that records the actor header of every inbound request.
  const filegate = Bun.serve({
    port: 0,
    fetch: (req) => {
      const url = new URL(req.url);
      seen.push({ actor: req.headers.get("x-filegate-actor"), path: url.pathname });
      if (url.pathname === "/v1/stats") {
        return Response.json({
          generatedAt: 0,
          index: { totalEntities: 0, totalFiles: 0, totalDirs: 0, dbSizeBytes: 0 },
          cache: { pathEntries: 0, pathCapacity: 0, pathUtilRatio: 0 },
          mounts: [],
          disks: [],
          system: { goroutines: 0, heapAllocBytes: 0, heapSysBytes: 0, heapObjects: 0, numGC: 0, lastGCPauseNs: 0, openFDs: 0, maxFDs: 0 },
        });
      }
      return Response.json({ items: [], total: 0 });
    },
  });

  Bun.env.FILEGATE_URL = `http://127.0.0.1:${filegate.port}`;
  Bun.env.FILEGATE_TOKEN = "filegate-token";
  Bun.env.ADMIN_TOKEN = "admin-token";
  Bun.env.ADMIN_SESSION_SECRET = "session-secret";
  delete Bun.env.REDIS_URL;

  app = (await import("../src/app")).app;
  issueSession = (await import("../src/lib/session")).issueSession;
});

function requestAs(label: string, path = "/"): Promise<Response> {
  const { value } = issueSession({ sub: "user-1", label, kind: "oidc" });
  return app.fetch(
    new Request(`http://localhost${path}`, {
      headers: { host: "localhost", cookie: `filegate_admin=${value}` },
      redirect: "manual",
    }),
  );
}

describe("actor propagation", () => {
  test("names the signed-in human on upstream requests", async () => {
    seen.length = 0;
    await requestAs("ada@example.com");

    expect(seen.length).toBeGreaterThan(0);
    expect(seen.every((call) => call.actor === "ada@example.com")).toBe(true);
  });

  test("keeps concurrent requests from different admins apart", async () => {
    // The actor lives in async-local storage, so overlapping requests must not
    // leak each other's identity.
    seen.length = 0;
    await Promise.all([requestAs("ada@example.com"), requestAs("grace@example.com")]);

    const actors = new Set(seen.map((call) => call.actor));
    expect(actors).toEqual(new Set(["ada@example.com", "grace@example.com"]));
  });
});
