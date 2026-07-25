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
  const clean = path.trim().replace(/^\/+|\/+$/g, "");
  if (!clean) {
    // The empty path addresses the list of mounts, which is not a directory
    // anything can be written into. Say so instead of "folder required".
    throw new Error("Target folder is required; pick a folder inside a mount, such as files or files/archive");
  }
  const out = await client().paths.get(clean);
  if (isList(out)) throw new Error(`"${clean}" is not a folder`);
  if (out.type !== "directory") throw new Error(`"${clean}" is a file, not a folder`);
  return out;
}
