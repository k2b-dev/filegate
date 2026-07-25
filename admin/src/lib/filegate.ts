import { Filegate, type Node, type NodeListResponse } from "@valentinkolb/filegate";
import { currentActor } from "./actor";
import { env } from "./env";

export function client(): Filegate {
  const cfg = env();
  const actor = currentActor();
  return new Filegate({
    baseUrl: cfg.filegateUrl,
    token: cfg.filegateToken,
    userAgent: "filegate-admin/0",
    // Names the human in Filegate's audit log instead of the shared token.
    // Filegate sanitizes and truncates the value on its side.
    defaultHeaders: actor ? { "X-Filegate-Actor": actor } : undefined,
  });
}

export function isList(value: Node | NodeListResponse): value is NodeListResponse {
  return "items" in value;
}

export function parentPath(path: string): string {
  const clean = path.trim().replace(/^\/+|\/+$/g, "");
  const idx = clean.lastIndexOf("/");
  return idx < 0 ? "" : clean.slice(0, idx);
}

export async function resolveDirectory(path: string): Promise<Node> {
  const out = await client().paths.get(path);
  if (isList(out)) throw new Error("folder required");
  if (out.type !== "directory") throw new Error("folder required");
  return out;
}
