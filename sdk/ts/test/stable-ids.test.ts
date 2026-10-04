import { expect, test } from "bun:test";
import { Filegate, FilegateError } from "../src/index";

const fileId = "0199afb0-7c00-7000-8000-000000000001";

test("ID reads and download leases keep the selector and backend execution scope", async () => {
  const calls: { url: URL; init?: RequestInit }[] = [];
  const request: typeof fetch = async (input, init) => {
    const url = new URL(String(input));
    calls.push({ url, init });
    if (url.pathname.endsWith("/content")) {
      return Response.json({ error: "not_found", message: "missing" }, { status: 404, headers: { "X-Filegate-Test": "preserved" } });
    }
    return Response.json({ url: "https://files.example/v1/direct/bound.signature", method: "GET", expires: "2030-01-01T00:00:00Z" }, { status: 201 });
  };
  const identity = { uid: 1001, gid: 100, groups: [200] };
  const root = new Filegate({ baseUrl: "https://files.example", token: "backend", fetch: request }).root("documents").as(identity);
  const controller = new AbortController();
  const response = await root.contentByIDRaw(fileId, controller.signal);
  expect(response.status).toBe(404);
  expect(response.headers.get("X-Filegate-Test")).toBe("preserved");
  expect(await response.json()).toEqual({ error: "not_found", message: "missing" });
  const lease = await root.directDownloadByID(fileId, { expiresIn: 30, fileName: "Grüße.txt" });
  expect(lease.method).toBe("GET");
  expect(lease.url).toBe("https://files.example/v1/direct/bound.signature");
  expect(calls).toHaveLength(2);
  expect(calls[0].url.pathname).toBe("/v1/roots/documents/content");
  expect([...calls[0].url.searchParams]).toEqual([["fileId", fileId]]);
  expect(calls[0].init?.method).toBe("GET");
  expect(calls[0].init?.body).toBeUndefined();
  expect(calls[0].init?.signal).toBe(controller.signal);
  expect(calls[1].url.pathname).toBe("/v1/roots/documents/downloads/direct");
  expect(calls[1].url.search).toBe("");
  expect(calls[1].init?.method).toBe("POST");
  expect(JSON.parse(String(calls[1].init?.body))).toEqual({ fileId, expiresIn: 30, fileName: "Grüße.txt" });
  for (const call of calls) {
    const headers = new Headers(call.init?.headers);
    expect(headers.get("Authorization")).toBe("Bearer backend");
    expect(headers.get("X-Filegate-Execution")).toBe(JSON.stringify(identity));
  }
});

test("ID download mint errors remain typed", async () => {
  const request: typeof fetch = async () => Response.json({ error: "feature_disabled", message: "stable IDs disabled" }, { status: 409 });
  const root = new Filegate({ baseUrl: "https://files.example", token: "backend", fetch: request }).root("documents");
  try {
    await root.directDownloadByID(fileId);
    throw Error("expected rejection");
  } catch (error) {
    expect(error).toBeInstanceOf(FilegateError);
    if (error instanceof FilegateError) {
      expect(error.status).toBe(409);
      expect(error.code).toBe("feature_disabled");
      expect(error.message).toBe("stable IDs disabled");
    }
  }
});
