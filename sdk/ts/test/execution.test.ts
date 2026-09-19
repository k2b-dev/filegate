import { expect, test } from "bun:test";
import { Filegate } from "../src/index";

test("execution clients are independent and attach identity only to backend operations", async () => {
  const calls: { url: string; headers: Headers }[] = [];
  const request: typeof fetch = async (input, init) => {
    calls.push({ url: String(input), headers: new Headers(init?.headers) });
    return Response.json(String(input).endsWith("/uploads/direct")
      ? { url: "https://direct.example/v1/direct/bound.signature", method: "PUT", expires: "2030-01-01" }
      : {});
  };
  const client = new Filegate({ baseUrl: "https://files.example", token: "backend", fetch: request });
  const groups = [200, 100, 200];
  const root = client.root("nfs").as({ uid: 1001, gid: 100, groups });
  groups.push(999);
  await root.stat("a");
  await client.root("nfs").stat("a");
  await root.put("a", new Blob(["test"]));
  expect(JSON.parse(calls[0].headers.get("X-Filegate-Execution")!)).toEqual({ uid: 1001, gid: 100, groups: [100, 200] });
  expect(calls[1].headers.has("X-Filegate-Execution")).toBe(false);
  expect(calls[2].headers.get("X-Filegate-Execution")).toBe(calls[0].headers.get("X-Filegate-Execution"));
  expect(calls[3].headers.has("Authorization")).toBe(false);
  expect(calls[3].headers.has("X-Filegate-Execution")).toBe(false);
});

test("execution validates numeric identity and bounds groups", () => {
  const client = new Filegate({ baseUrl: "https://files.example", token: "backend" });
  for (const uid of [0, -1, 1.5, NaN, Infinity, 0xffffffff]) expect(() => client.as({ uid, gid: 100 })).toThrow();
  for (const gid of [-1, 1.5, NaN, Infinity, 0xffffffff]) expect(() => client.as({ uid: 1001, gid })).toThrow();
  expect(() => client.as({ uid: 1001, gid: 100, groups: Array(65).fill(100) })).toThrow();
  expect(() => client.as({ uid: 1001, gid: 100, groups: [0xffffffff] })).toThrow();
});

test("transfers and historical copies bind destination separately from source", async () => {
  const calls: { headers: Headers; body: unknown }[] = [];
  const request: typeof fetch = async (_input, init) => {
    calls.push({ headers: new Headers(init?.headers), body: JSON.parse(String(init?.body)) });
    return Response.json({});
  };
  const client = new Filegate({ baseUrl: "https://files.example", token: "backend", fetch: request });
  const identity = { uid: 1001, gid: 100, groups: [200] };
  await client.root("ipa").as(identity).transfer("a", "cloud", "b", { targetExecution: { mode: "service" } });
  await client.root("cloud").copyVersion("a", "version", "ipa", "b", { targetExecution: { mode: "unix", identity } });
  expect(calls[0].headers.get("X-Filegate-Execution")).toBe(JSON.stringify(identity));
  expect(calls[0].body).toEqual({ path: "a", targetRoot: "cloud", targetPath: "b", targetExecution: { mode: "service" } });
  expect(calls[1].headers.has("X-Filegate-Execution")).toBe(false);
  expect(calls[1].body).toEqual({ path: "a", targetRoot: "ipa", targetPath: "b", targetExecution: { mode: "unix", identity } });
});
