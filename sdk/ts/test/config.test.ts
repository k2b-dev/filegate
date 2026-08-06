import { describe, expect, test } from "bun:test";
import { Filegate } from "../src/client.js";
import { FilegateError } from "../src/errors.js";

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

describe("S3 keys client", () => {
  test("covers the key lifecycle and rejects delete errors", async () => {
    let failDelete = false;
    const requests: string[] = [];
    const client = new Filegate({
      baseUrl: "https://filegate.example",
      token: "secret",
      fetchImpl: async (input, init) => {
        const url = new URL(String(input));
        requests.push(`${init?.method} ${url.pathname}`);
        if (init?.method === "DELETE") {
          return failDelete
            ? Response.json({ error: "key not found" }, { status: 404 })
            : new Response(null, { status: 204 });
        }
        if (url.pathname.endsWith("/rotate")) {
          return Response.json({ accessKey: "key/one", secretKey: "rotated", buckets: ["data"] });
        }
        if (init?.method === "PATCH") {
          return Response.json({ accessKey: "key/one", buckets: ["data"], disabled: true });
        }
        if (init?.method === "POST") {
          return Response.json({ accessKey: "key/one", secretKey: "created", buckets: ["data"] });
        }
        return Response.json({ items: [{ accessKey: "key/one", buckets: ["data"] }], total: 1 });
      },
    });

    expect((await client.s3Keys.list()).total).toBe(1);
    expect((await client.s3Keys.create({ buckets: ["data"] })).secretKey).toBe("created");
    expect((await client.s3Keys.update("key/one", { disabled: true })).disabled).toBe(true);
    expect((await client.s3Keys.rotate("key/one")).secretKey).toBe("rotated");
    await expect(client.s3Keys.delete("key/one")).resolves.toBeUndefined();
    expect(requests).toEqual([
      "GET /v1/s3/keys",
      "POST /v1/s3/keys",
      "PATCH /v1/s3/keys/key%2Fone",
      "POST /v1/s3/keys/key%2Fone/rotate",
      "DELETE /v1/s3/keys/key%2Fone",
    ]);

    failDelete = true;
    try {
      await client.s3Keys.delete("missing");
      throw new Error("expected delete to fail");
    } catch (error) {
      expect(error).toBeInstanceOf(FilegateError);
      expect((error as FilegateError).status).toBe(404);
      expect((error as FilegateError).path).toBe("/v1/s3/keys/missing");
    }
  });
});
