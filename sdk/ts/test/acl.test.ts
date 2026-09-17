import { expect, test } from "bun:test";
import { Filegate, FilegateError, type ACL } from "../src/index";

const acl: ACL = { entries: [
  { tag: "owner", permissions: "rwx" },
  { tag: "user", id: 0, permissions: "r-x" },
  { tag: "owningGroup", permissions: "rwx" },
  { tag: "mask", permissions: "rwx" },
  { tag: "other", permissions: "---" },
] };

test("ACL methods preserve scope, encoded paths, numeric IDs, and authentication", async () => {
  const methods: string[] = [];
  const request: typeof fetch = async (input, init) => {
    const url = new URL(String(input));
    methods.push(init?.method ?? "GET");
    expect(url.pathname).toBe("/v1/roots/freeipa/acl");
    expect(url.searchParams.get("path")).toBe("groups/a & b");
    expect(url.searchParams.get("scope")).toBe("default");
    expect(new Headers(init?.headers).get("Authorization")).toBe("Bearer secret");
    if (init?.method === "PUT") {
      expect(new Headers(init.headers).get("Content-Type")).toBe("application/json");
      expect(JSON.parse(String(init.body))).toEqual(acl);
    }
    return init?.method === "DELETE" ? new Response(null, { status: 204 }) : Response.json(acl);
  };
  const root = new Filegate({ baseUrl: "https://files.example", token: "secret", fetch: request }).root("freeipa");
  expect(await root.getACL("groups/a & b", "default")).toEqual(acl);
  expect(await root.setACL("groups/a & b", "default", acl)).toEqual(acl);
  expect(await root.clearDefaultACL("groups/a & b")).toBeUndefined();
  expect(methods).toEqual(["GET", "PUT", "DELETE"]);
});

test("ACL methods preserve unsupported errors instead of reporting an absent ACL", async () => {
  const request: typeof fetch = async (input) => {
    expect(new URL(String(input)).searchParams.get("scope")).toBe("access");
    return Response.json({ error: "acl_not_supported", message: "POSIX ACLs are not supported by this filesystem or mount" }, { status: 501 });
  };
  const root = new Filegate({ baseUrl: "https://files.example", token: "secret", fetch: request }).root("freeipa");
  try {
    await root.getACL(".", "access");
    throw new Error("expected rejection");
  } catch (error) {
    expect(error).toBeInstanceOf(FilegateError);
    if (error instanceof FilegateError) {
      expect(error.status).toBe(501);
      expect(error.code).toBe("acl_not_supported");
      expect(error.message).toContain("POSIX ACLs");
    }
  }
});
