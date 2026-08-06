import { describe, expect, test } from "bun:test";
import { Filegate } from "../src/client.js";

describe("system client", () => {
  test("covers operational endpoints", async () => {
    const requests: string[] = [];
    const client = new Filegate({
      baseUrl: "https://filegate.example",
      token: "secret",
      fetchImpl: async (input, init) => {
        const url = new URL(String(input));
        requests.push(`${init?.method} ${url.pathname}${url.search}`);
        if (url.pathname.endsWith("/info")) return Response.json({ mounts: [] });
        if (url.pathname.endsWith("/runtime")) return Response.json({ uploadSessions: {} });
        if (url.pathname.endsWith("/health")) return Response.json({ status: "ok", checks: [] });
        if (url.pathname.endsWith("/prune")) return Response.json({ filesScanned: 2 });
        return Response.json({ items: [], total: 0 });
      },
    });

    expect((await client.system.info()).mounts).toEqual([]);
    expect((await client.system.runtime()).uploadSessions).toEqual({});
    expect((await client.system.health()).status).toBe("ok");
    expect((await client.system.prune()).filesScanned).toBe(2);
    expect((await client.system.uploadSessions({ phase: "in_progress" })).total).toBe(0);
    expect(requests).toEqual([
      "GET /v1/system/info",
      "GET /v1/system/runtime",
      "GET /v1/health",
      "POST /v1/versions/prune",
      "GET /v1/uploads/sessions?phase=in_progress",
    ]);
  });
});
