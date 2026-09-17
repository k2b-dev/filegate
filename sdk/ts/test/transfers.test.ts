import { expect, test } from "bun:test";
import { Filegate, type ArchiveLease } from "../src/index";
import { archiveRaw, DirectSession } from "../src/utils";

test("backend owns session lifecycle, leases, and directory provisioning", async () => {
  const calls: { path: string; method: string; body: unknown }[] = [];
  const request: typeof fetch = async (input, init) => {
    expect(new Headers(init?.headers).get("Authorization")).toBe("Bearer backend");
    calls.push({ path: new URL(String(input)).pathname, method: init?.method ?? "GET", body: init?.body ? JSON.parse(String(init.body)) : undefined });
    return init?.method === "DELETE" ? new Response(null, { status: 204 }) : Response.json({});
  };
  const root = new Filegate({ baseUrl: "https://files.example", token: "backend", fetch: request }).root("cloud");
  await root.createSession("inbox/new", 0, { expiresIn: 30, allowAbort: true, onConflict: "error", ownership: { uid: 12, gid: 34 } });
  await root.session("session-id");
  await root.sessionLease("session-id", { expiresIn: 60 });
  await root.commitSession("session-id");
  await root.abortSession("session-id");
  await root.mkdir("groups/editors", { ownership: { dirMode: "2770" }, acl: { default: { entries: [] } } });
  expect(calls).toEqual([
    { path: "/v1/roots/cloud/uploads/sessions", method: "POST", body: { path: "inbox/new", size: 0, expiresIn: 30, allowAbort: true, onConflict: "error", ownership: { uid: 12, gid: 34 } } },
    { path: "/v1/roots/cloud/uploads/sessions/session-id", method: "GET", body: undefined },
    { path: "/v1/roots/cloud/uploads/sessions/session-id/lease", method: "POST", body: { expiresIn: 60 } },
    { path: "/v1/roots/cloud/uploads/sessions/session-id/commit", method: "POST", body: undefined },
    { path: "/v1/roots/cloud/uploads/sessions/session-id", method: "DELETE", body: undefined },
    { path: "/v1/roots/cloud/directories", method: "POST", body: { path: "groups/editors", ownership: { dirMode: "2770" }, acl: { default: { entries: [] } } } },
  ]);
});

test("empty browser upload stays open and never commits", async () => {
  const request: typeof fetch = async (_input, init) => {
    expect(init?.method).toBe("GET");
    return Response.json({ state: "open", size: 0, received: 0, chunkSize: 8, segments: {} });
  };
  const session = new DirectSession("https://files.example/v1/direct/session", request);
  expect("commit" in session).toBe(false);
  expect(await session.upload(new Blob())).toMatchObject({ state: "open", received: 0 });
});

test("terminal browser session does not write or attempt to publish", async () => {
  const request: typeof fetch = async (_input, init) => {
    expect(init?.method).toBe("GET");
    return Response.json({ state: "aborted", size: 5, received: 0, chunkSize: 8, segments: {} });
  };
  const session = new DirectSession("https://files.example/v1/direct/session", request);
  expect(await session.upload(new Blob(["hello"]))).toMatchObject({ state: "aborted" });
});

test("archive mint is authenticated, download posts exact manifest without bearer and preserves errors", async () => {
  const lease: ArchiveLease = { url: "https://download.example/v1/direct/archive", method: "POST", expires: "2026-09-18T00:00:00Z", manifest: '{"items":[{"path":"a & b"}]}' };
  const request: typeof fetch = async (input, init) => {
    if (String(input).endsWith("/downloads/archives")) {
      expect(new Headers(init?.headers).get("Authorization")).toBe("Bearer backend");
      expect(JSON.parse(String(init?.body))).toEqual({ items: [{ root: "cloud", path: "a & b", archivePath: "files/a & b" }], expiresIn: 30 });
      return Response.json(lease);
    }
    expect(String(input)).toBe(lease.url);
    expect(init?.method).toBe("POST");
    expect(init?.redirect).toBe("error");
    expect(new Headers(init?.headers).has("Authorization")).toBe(false);
    expect(init?.body).toBeInstanceOf(URLSearchParams);
    expect(new URLSearchParams(String(init?.body)).get("manifest")).toBe(lease.manifest);
    return Response.json({ error: "lease_expired" }, { status: 403 });
  };
  const client = new Filegate({ baseUrl: "https://files.example", token: "backend", fetch: request });
  const minted = await client.archiveLease([{ root: "cloud", path: "a & b", archivePath: "files/a & b" }], 30);
  expect((await client.archiveRaw(minted)).status).toBe(403);
  expect((await archiveRaw(minted, { fetch: request })).status).toBe(403);
});
