import { describe, expect, test } from "bun:test";
import { FilegateError } from "../src/errors.js";
import { directUploads, upload, uploadDirect } from "../src/uploads.js";
import { uploads } from "../src/utils.js";
import type { Node } from "../src/types.js";

const node: Node = {
  id: "node-1",
  type: "file",
  name: "hello.txt",
  path: "data/hello.txt",
  size: 5,
  mtime: 1,
  ownership: { uid: 1000, gid: 1000, mode: "644" },
  exif: {},
};

describe("uploadDirect", () => {
  test("uploads with a presigned URL and runs success/finish callbacks", async () => {
    const calls: string[] = [];
    const result = await uploadDirect("https://files.example.test/v1/uploads/direct/token", new Blob(["hello"], { type: "text/plain" }), {
      fetchImpl: async (input, init) => {
        expect(String(input)).toBe("https://files.example.test/v1/uploads/direct/token");
        expect(init?.method).toBe("PUT");
        expect((init?.headers as Record<string, string>)["Content-Type"]).toMatch(/^text\/plain/);
        return new Response(JSON.stringify(node), {
          status: 201,
          headers: {
            "Content-Type": "application/json",
            "X-Node-Id": "node-1",
            "X-Created-Id": "node-1",
          },
        });
      },
      onSuccess: async (res) => {
        calls.push(`success:${res.node.id}`);
      },
      onError: async () => {
        calls.push("error");
      },
      onFinish: async (outcome) => {
        calls.push(outcome.ok ? "finish:ok" : "finish:error");
      },
    });

    expect(result.nodeId).toBe("node-1");
    expect(result.statusCode).toBe(201);
    expect(calls).toEqual(["success:node-1", "finish:ok"]);
  });

  test("reports server errors to onError and onFinish", async () => {
    const calls: string[] = [];
    const err = await uploadDirect("https://files.example.test/v1/uploads/direct/token", "hello", {
      fetchImpl: async () =>
        new Response(JSON.stringify({ error: "path already exists", existingId: "old" }), {
          status: 409,
          statusText: "Conflict",
          headers: { "Content-Type": "application/json" },
        }),
      onError: async (error) => {
        calls.push(error instanceof FilegateError ? `error:${error.status}` : "error:unknown");
      },
      onFinish: async (outcome) => {
        calls.push(outcome.ok ? "finish:ok" : "finish:error");
      },
    }).catch((error) => error);

    expect(err).toBeInstanceOf(FilegateError);
    expect((err as FilegateError).status).toBe(409);
    expect(calls).toEqual(["error:409", "finish:error"]);
  });
});

describe("directUploads", () => {
  const direct = {
    baseUrl: "https://files.example.test/v1/uploads/sessions/upl_abc",
    token: "session-token",
    expiresAt: 1,
    allow: ["putSegment", "status", "commit", "abort"] as const,
  };

  test("puts a segment with a scoped session token", async () => {
    const result = await directUploads.segments.put({
      direct,
      index: 2,
      body: new Blob(["chunk"]),
      checksum: "sha256:" + "a".repeat(64),
      fetchImpl: async (input, init) => {
        expect(String(input)).toBe("https://files.example.test/v1/uploads/sessions/upl_abc/segments/2");
        expect(init?.method).toBe("PUT");
        const headers = init?.headers as Record<string, string>;
        expect(headers["Filegate-Upload-Session"]).toBe("session-token");
        expect(headers["X-Segment-Checksum"]).toBe("sha256:" + "a".repeat(64));
        return new Response(JSON.stringify({
          sessionId: "upl_abc",
          index: 2,
          uploadedSegments: [2],
        }), { status: 200, headers: { "Content-Type": "application/json" } });
      },
    });

    expect(result.uploadedSegments).toEqual([2]);
  });

  test("commits a direct session", async () => {
    const result = await directUploads.commit({
      direct,
      fetchImpl: async (input, init) => {
        expect(String(input)).toBe("https://files.example.test/v1/uploads/sessions/upl_abc/commit");
        expect(init?.method).toBe("POST");
        expect((init?.headers as Record<string, string>)["Filegate-Upload-Session"]).toBe("session-token");
        return new Response(JSON.stringify({ node, checksum: "sha256:" + "b".repeat(64) }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      },
    });

    expect(result.node.id).toBe("node-1");
  });
});

describe("upload", () => {
  test("uploads small files with direct signed URLs without hashing first", async () => {
    const events: string[] = [];
    const result = await upload({
      files: [new File(["hello"], "small.txt", { type: "text/plain" })],
      path: "data",
      config: { directThresholdBytes: 10, batch: { size: 10, flushMs: 0 } },
      allow: async ({ uploads: requested }) => {
        expect(requested).toHaveLength(1);
        expect(requested[0].kind).toBe("direct");
        expect(requested[0].checksum).toBeUndefined();
        expect(requested[0].segments).toBeUndefined();
        return {
          uploads: [{
            id: requested[0].id,
            ok: true,
            upload: {
              kind: "direct",
              direct: {
                uploadUrl: "https://files.example.test/v1/uploads/direct/token",
                method: "PUT",
                path: requested[0].path,
                expiresAt: 1,
                maxBytes: 10,
              },
            },
          }],
        };
      },
      fetchImpl: async (input, init) => {
        expect(String(input)).toBe("https://files.example.test/v1/uploads/direct/token");
        expect(init?.method).toBe("PUT");
        return new Response(JSON.stringify(node), {
          status: 201,
          headers: {
            "Content-Type": "application/json",
            "X-Node-Id": "node-1",
            "X-Created-Id": "node-1",
          },
        });
      },
      onEvent: (event) => {
        events.push(event.type);
      },
    });

    expect(result).toEqual({ total: 1, done: 1, failed: 0, rejected: 0, skipped: 0 });
    expect(events).not.toContain("file:hashing");
    expect(events).toContain("file:done");
  });

  test("uploads allowed files and keeps per-file rejections local", async () => {
    const events: string[] = [];
    const result = await upload({
      files: [new File(["hello"], "ok.txt", { type: "text/plain" }), new File(["blocked"], "blocked.txt")],
      path: "data",
      config: { segmentSize: 4, chunkSize: 2, batch: { size: 10, flushMs: 0 } },
      allow: async ({ uploads: requested }) => ({
        uploads: requested.map((item) =>
          item.name === "blocked.txt"
            ? { id: item.id, ok: false, error: "blocked" }
            : {
                id: item.id,
                ok: true,
                session: {
                  id: "upl_1",
                  path: item.path,
                  size: item.size,
                  checksum: item.checksum,
                  segmentSize: item.segmentSize,
                  totalSegments: item.segments.length,
                  segments: item.segments.map(({ index, offset, size }) => ({ index, offset, size })),
                  uploadedSegments: [],
                  phase: "in_progress",
                  direct: {
                    baseUrl: "https://files.example.test/v1/uploads/sessions/upl_1",
                    token: "session-token",
                    expiresAt: 1,
                    allow: ["putSegment", "commit"],
                  },
                },
              },
        ),
      }),
      fetchImpl: async (input, init) => {
        if (String(input).endsWith("/commit")) {
          return new Response(JSON.stringify({ node, checksum: "sha256:" + "b".repeat(64) }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          });
        }
        expect(init?.method).toBe("PUT");
        expect((init?.headers as Record<string, string>)["Filegate-Upload-Session"]).toBe("session-token");
        expect((init?.headers as Record<string, string>)["X-Segment-Checksum"]).toMatch(/^sha256:/);
        return new Response(JSON.stringify({ sessionId: "upl_1", index: 0, uploadedSegments: [0] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      },
      onEvent: (event) => {
        if (event.type === "file:done" || event.type === "file:rejected") events.push(event.type);
      },
    });

    expect(result).toEqual({ total: 2, done: 1, failed: 0, rejected: 1, skipped: 0 });
    expect(events.sort()).toEqual(["file:done", "file:rejected"]);
  });

  test("counts skipped allow results separately from failures", async () => {
    const events: string[] = [];
    const result = await upload({
      files: [new File(["hello"], "exists.txt", { type: "text/plain" })],
      path: "data",
      config: { directThresholdBytes: 10, batch: { size: 10, flushMs: 0 } },
      allow: async ({ uploads: requested, onConflict }) => {
        expect(onConflict).toBe("skip-existing");
        return {
          uploads: [{
            id: requested[0].id,
            ok: false,
            skipped: true,
            error: "path already exists",
            code: "already_exists",
          }],
        };
      },
      onEvent: (event) => {
        if (event.type === "file:skipped" || event.type === "file:error") events.push(event.type);
      },
    });

    expect(result).toEqual({ total: 1, done: 0, failed: 0, rejected: 0, skipped: 1 });
    expect(events).toEqual(["file:skipped"]);
  });

  test("flushes allow batches before all files are collected", async () => {
    const batches: string[][] = [];
    await upload({
      files: [new File(["a"], "a.txt"), new File(["b"], "b.txt")],
      path: "data",
      config: { segmentSize: 8, chunkSize: 8, batch: { size: 1, flushMs: 0 } },
      allow: async ({ uploads: requested }) => {
        batches.push(requested.map((item) => item.name));
        return {
          uploads: requested.map((item) => ({
            id: item.id,
            ok: true,
            session: {
              id: `upl_${item.id}`,
              path: item.path,
              size: item.size,
              checksum: item.checksum,
              segmentSize: item.segmentSize,
              totalSegments: item.segments.length,
              segments: item.segments.map(({ index, offset, size }) => ({ index, offset, size })),
              uploadedSegments: [],
              phase: "in_progress",
              direct: {
                baseUrl: `https://files.example.test/v1/uploads/sessions/upl_${item.id}`,
                token: "session-token",
                expiresAt: 1,
                allow: ["putSegment", "commit"],
              },
            },
          })),
        };
      },
      fetchImpl: async (input) => {
        if (String(input).endsWith("/commit")) {
          return new Response(JSON.stringify({ node, checksum: "sha256:" + "b".repeat(64) }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          });
        }
        return new Response(JSON.stringify({ sessionId: "upl", index: 0, uploadedSegments: [0] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      },
    });

    expect(batches).toEqual([["a.txt"], ["b.txt"]]);
  });

  test("uses the conservative Filegate-compatible segment size by default", async () => {
    let seenSegmentSize = 0;
    await upload({
      files: [new File(["a"], "a.txt")],
      path: "data",
      // The segment size is only observable on a session request, and a
      // one-byte file now goes direct by default.
      config: { directThresholdBytes: 0 },
      allow: async ({ uploads: requested }) => {
        seenSegmentSize = requested[0].segmentSize;
        return {
          uploads: requested.map((item) => ({
            id: item.id,
            ok: true,
            session: {
              id: `upl_${item.id}`,
              path: item.path,
              size: item.size,
              checksum: item.checksum,
              segmentSize: item.segmentSize,
              totalSegments: item.segments.length,
              segments: item.segments.map(({ index, offset, size }) => ({ index, offset, size })),
              uploadedSegments: [],
              phase: "in_progress",
              direct: {
                baseUrl: `https://files.example.test/v1/uploads/sessions/upl_${item.id}`,
                token: "session-token",
                expiresAt: 1,
                allow: ["putSegment", "commit"],
              },
            },
          })),
        };
      },
      fetchImpl: async (input) => {
        if (String(input).endsWith("/commit")) {
          return new Response(JSON.stringify({ node, checksum: "sha256:" + "b".repeat(64) }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          });
        }
        return new Response(JSON.stringify({ sessionId: "upl", index: 0, uploadedSegments: [0] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      },
    });

    expect(seenSegmentSize).toBe(8 * 1024 * 1024);
  });

  test("limits segment uploads globally across files", async () => {
    let active = 0;
    let maxActive = 0;
    await upload({
      files: [new File(["abcd"], "a.txt"), new File(["efgh"], "b.txt")],
      path: "data",
      config: {
        segmentSize: 1,
        chunkSize: 1,
        concurrency: { hash: 2, files: 2, segments: 1 },
        batch: { size: 2, flushMs: 0 },
      },
      allow: async ({ uploads: requested }) => ({
        uploads: requested.map((item) => ({
          id: item.id,
          ok: true,
          session: {
            id: `upl_${item.id}`,
            path: item.path,
            size: item.size,
            checksum: item.checksum,
            segmentSize: item.segmentSize,
            totalSegments: item.segments.length,
            segments: item.segments.map(({ index, offset, size }) => ({ index, offset, size })),
            uploadedSegments: [],
            phase: "in_progress",
            direct: {
              baseUrl: `https://files.example.test/v1/uploads/sessions/upl_${item.id}`,
              token: "session-token",
              expiresAt: 1,
              allow: ["putSegment", "commit"],
            },
          },
        })),
      }),
      fetchImpl: async (input) => {
        if (String(input).endsWith("/commit")) {
          return new Response(JSON.stringify({ node, checksum: "sha256:" + "b".repeat(64) }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          });
        }
        active++;
        maxActive = Math.max(maxActive, active);
        await new Promise((resolve) => setTimeout(resolve, 2));
        active--;
        return new Response(JSON.stringify({ sessionId: "upl", index: 0, uploadedSegments: [0] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      },
    });

    expect(maxActive).toBe(1);
  });
});

describe("uploads utilities", () => {
  test("plans segments", () => {
    expect(uploads.segments.plan({ size: 10, segmentSize: 4 })).toEqual([
      { index: 0, offset: 0, size: 4 },
      { index: 1, offset: 4, size: 4 },
      { index: 2, offset: 8, size: 2 },
    ]);
  });
});

describe("upload direct threshold", () => {
  // Files at or below the segment size take one PUT instead of a session:
  // three requests and a resumability guarantee that amounts to retrying the
  // same lone segment buys nothing for a 16 KiB file.
  test("routes small files direct by default", async () => {
    const kinds: string[] = [];
    await upload({
      files: [new File(["a"], "a.txt")],
      path: "data",
      allow: async ({ uploads: requested }) => {
        for (const item of requested) kinds.push(item.kind);
        return {
          uploads: requested.map((item) => ({
            id: item.id,
            ok: true as const,
            upload: {
              kind: "direct" as const,
              direct: {
                uploadUrl: "https://files.example.test/v1/uploads/direct/tok",
                method: "PUT" as const,
                path: item.path,
                expiresAt: 1,
                maxBytes: 1024,
              },
            },
          })),
        };
      },
      fetchImpl: async () =>
        new Response(JSON.stringify(node), { status: 201, headers: { "Content-Type": "application/json" } }),
    });
    expect(kinds).toEqual(["direct"]);
  });

  // Zero is the opt-out. It reads as "no file is small enough", which is
  // exactly the old behaviour for callers that want every upload resumable.
  test("directThresholdBytes 0 forces sessions", async () => {
    const kinds: string[] = [];
    await upload({
      files: [new File(["a"], "a.txt")],
      path: "data",
      config: { directThresholdBytes: 0 },
      allow: async ({ uploads: requested }) => {
        for (const item of requested) kinds.push(item.kind);
        return {
          uploads: requested.map((item) => ({
            id: item.id,
            ok: true as const,
            session: {
              id: `upl_${item.id}`,
              path: item.path,
              size: item.size,
              checksum: item.checksum!,
              segmentSize: item.segmentSize!,
              totalSegments: item.segments!.length,
              segments: item.segments!.map(({ index, offset, size }) => ({ index, offset, size })),
              uploadedSegments: [],
              phase: "in_progress" as const,
              direct: {
                baseUrl: `https://files.example.test/v1/uploads/sessions/upl_${item.id}`,
                token: "session-token",
                expiresAt: 1,
                allow: ["putSegment", "commit"],
              },
            },
          })),
        };
      },
      fetchImpl: async (input) =>
        String(input).endsWith("/commit")
          ? new Response(JSON.stringify({ node, checksum: "sha256:" + "b".repeat(64) }), {
              status: 200,
              headers: { "Content-Type": "application/json" },
            })
          : new Response(JSON.stringify({ sessionId: "upl", index: 0, uploadedSegments: [0] }), {
              status: 200,
              headers: { "Content-Type": "application/json" },
            }),
    });
    expect(kinds).toEqual(["session"]);
  });
});
