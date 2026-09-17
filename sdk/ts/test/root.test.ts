import { describe, expect, test } from "bun:test";
import { Filegate, FilegateError } from "../src/index";
import { DirectSession, segments, sha256 } from "../src/utils";

describe("root contract", () => {
  test("direct PUT carries ownership and metadata only in the authenticated mint", async () => {
    const calls: { url: string; init?: RequestInit }[] = [];
    const request: typeof fetch = async (input, init) => {
      const url = String(input); calls.push({ url, init });
      return Response.json(url.endsWith("/uploads/direct")
        ? { url: "https://files.example/v1/direct/scoped", method: "PUT", expires: "2026-09-16T20:00:00Z" }
        : { root: "cloud", path: "a.txt", size: 5, directory: false });
    };
    const files = new Filegate({ baseUrl: "https://files.example", token: "backend-secret", fetch: request });
    await files.root("cloud").put("a.txt", new Blob(["hello"]), { ownership: { uid: 12, gid: 34, mode: "0640" }, metadata: { message: "hello" } });
    expect(calls).toHaveLength(2);
    expect(new Headers(calls[0].init?.headers).get("Authorization")).toBe("Bearer backend-secret");
    expect(JSON.parse(String(calls[0].init?.body))).toMatchObject({ size: 5, ownership: { uid: 12, gid: 34 }, metadata: { message: "hello" } });
    expect(new Headers(calls[1].init?.headers).has("Authorization")).toBe(false);
  });
  test("raw methods preserve errors; typed methods expose status and code", async () => {
    const request: typeof fetch = async () => Response.json({ error: "conflict", message: "occupied" }, { status: 409 });
    const root = new Filegate({ baseUrl: "https://files.example", token: "secret", fetch: request }).root("cloud");
    expect((await root.contentRaw("a")).status).toBe(409);
    try { await root.stat("a"); throw Error("expected rejection"); } catch (e) { expect(e).toBeInstanceOf(FilegateError); if (e instanceof FilegateError) { expect(e.status).toBe(409); expect(e.code).toBe("conflict"); } }
  });
  test("browser session resumes received segments and sends no bearer", async () => {
    const methods: string[] = [];
    const request: typeof fetch = async (_input, init) => {
      expect(new Headers(init?.headers).has("Authorization")).toBe(false);
      methods.push(init?.method ?? "GET");
      if (init?.method === "GET") return Response.json({ state: "open", size: 5, chunkSize: 3, received: 3, segments: { "0": await sha256(new TextEncoder().encode("hel")) } });
      return Response.json({ state: "open", size: 5, chunkSize: 3, received: 5, segments: {} });
    };
    await new DirectSession("https://files.example/v1/direct/session", request).upload(new Blob(["hello"]));
    expect(methods).toEqual(["GET", "PUT"]);
    expect(segments(5, 3)).toEqual([{ index: 0, offset: 0, size: 3 }, { index: 1, offset: 3, size: 2 }]);
  });
  test("backend raw requests cannot send credentials to another origin", async () => {
    const client = new Filegate({ baseUrl: "https://files.example", token: "secret" });
    await expect(client.raw("GET", "https://other.example/")).rejects.toThrow("origin");
  });
});

test("resuming with different same-size file stops before uploading", async () => {
  const methods: string[] = [];
  const request: typeof fetch = async (_input, init) => {
    methods.push(init?.method ?? "GET");
    return Response.json({ state: "open", size: 6, chunkSize: 3, received: 3, segments: { "0": await sha256(new TextEncoder().encode("AAA")) } });
  };
  await expect(new DirectSession("https://files.example/v1/direct/session", request).upload(new Blob(["BBBBBB"]))).rejects.toThrow("differ");
  expect(methods).toEqual(["GET"]);
});
