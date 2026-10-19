import { expect, test } from "bun:test";
import { Filegate } from "../src/index";

test("search forwards metadata filters with an empty query and execution identity", async () => {
  const calls: { url: URL; execution: string | null }[] = [];
  const request: typeof fetch = async (input, init) => {
    calls.push({ url: new URL(String(input)), execution: new Headers(init?.headers).get("X-Filegate-Execution") });
    return Response.json({ items: [] });
  };
  const root = new Filegate({ baseUrl: "https://api.test", token: "backend", fetch: request }).root("files");
  const options = { ext: "PDF,docx", minSize: 0, maxSize: 10485760, modifiedAfter: "2026-01-01T00:00:00Z", modifiedBefore: "2027-01-01T00:00:00.123456789Z" };
  await root.search("", options);
  await root.as({ uid: 1001, gid: 100 }).search("", options);
  for (const call of calls) {
    expect(call.url.pathname).toBe("/v1/roots/files/search");
    expect(Object.fromEntries(call.url.searchParams)).toEqual({ q: "", ...Object.fromEntries(Object.entries(options).map(([key, value]) => [key, String(value)])) });
  }
  expect(calls[0].execution).toBeNull();
  expect(JSON.parse(calls[1].execution!)).toMatchObject({ uid: 1001, gid: 100 });
});

test("search omits unspecified metadata filters", async () => {
  let url: URL | undefined;
  const request: typeof fetch = async input => { url = new URL(String(input)); return Response.json({ items: [] }); };
  await new Filegate({ baseUrl: "https://api.test", token: "backend", fetch: request }).root("files").search("");
  expect(Object.fromEntries(url!.searchParams)).toEqual({ q: "" });
});
