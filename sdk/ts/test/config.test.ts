import { describe, expect, test } from "bun:test";
import { Filegate } from "../src/client.js";

describe("declarative config client", () => {
  test("plans and applies complete manifests with revision preconditions", async () => {
    const requests: { path: string; method?: string; body?: string }[] = [];
    const client = new Filegate({
      baseUrl: "https://filegate.example",
      token: "secret",
      fetchImpl: async (input, init) => {
        const url = new URL(String(input));
        requests.push({ path: url.pathname, method: init?.method, body: String(init?.body ?? "") });
        if (url.pathname.endsWith("/plan")) {
          return Response.json({ currentRevision: "old", proposedRevision: "new", changes: [] });
        }
        return Response.json({
          manifest: { revision: "new", appliedAt: 1, appliedBy: "test" },
          changes: [],
        });
      },
    });

    await client.config.plan({ "upload.max_upload_bytes": 4096 });
    await client.config.apply({ "upload.max_upload_bytes": 4096 }, "old");

    expect(requests.map((request) => request.path)).toEqual(["/v1/config/plan", "/v1/config/apply"]);
    expect(JSON.parse(requests[0]!.body!)).toEqual({ values: { "upload.max_upload_bytes": 4096 } });
    expect(JSON.parse(requests[1]!.body!)).toEqual({
      values: { "upload.max_upload_bytes": 4096 },
      expectedRevision: "old",
    });
  });
});
