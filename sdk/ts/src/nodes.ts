import { ClientCore } from "./core.js";
import { ensureSuccess } from "./errors.js";
import type { GetPathOptions } from "./paths.js";
import type { MkdirRequest, Node, UpdateNodeRequest } from "./types.js";

export interface ThumbnailOptions {
  /** 128, 256 or 512; the server rejects anything else. */
  size?: number;
  /**
   * Previously received ETag. The server answers 304 when the thumbnail is
   * unchanged, which avoids a re-encode and lets a proxying caller pass the
   * validator straight through to a browser cache.
   */
  ifNoneMatch?: string;
}

function getQuery(opts?: GetPathOptions): Record<string, string> {
  const q: Record<string, string> = {};
  if (opts?.pageSize) q["pageSize"] = String(opts.pageSize);
  if (opts?.cursor) q["cursor"] = opts.cursor;
  if (opts?.computeRecursiveSizes) q["computeRecursiveSizes"] = "true";
  return q;
}

export class NodesClient {
  constructor(private readonly core: ClientCore) {}

  /** Get metadata for a node by ID. For directories, children can be paged. */
  async get(id: string, opts?: GetPathOptions): Promise<Node> {
    if (!id.trim()) throw new Error("id is required");
    return this.core.doJSON<Node>("GET", `/v1/nodes/${encodeURIComponent(id)}`, getQuery(opts));
  }

  /** Stream file content or tar archive (for directories). Returns raw Response. */
  async contentRaw(id: string, opts?: { inline?: boolean }): Promise<Response> {
    if (!id.trim()) throw new Error("id is required");
    const q: Record<string, string> = {};
    if (opts?.inline) q["inline"] = "true";
    return this.core.doRaw("GET", `/v1/nodes/${encodeURIComponent(id)}/content`, q);
  }

  /** Replace content of a file node. */
  async putContent(id: string, data: BodyInit, contentType?: string): Promise<Node> {
    if (!id.trim()) throw new Error("id is required");
    return this.core.doJSON<Node>(
      "PUT",
      `/v1/nodes/${encodeURIComponent(id)}`,
      undefined,
      data,
      contentType ?? "application/octet-stream",
    );
  }

  /** Create a subdirectory under a parent node. */
  async mkdir(parentId: string, req: MkdirRequest): Promise<Node> {
    if (!parentId.trim()) throw new Error("parent id is required");
    return this.core.doJSON<Node>(
      "POST",
      `/v1/nodes/${encodeURIComponent(parentId)}/mkdir`,
      undefined,
      JSON.stringify(req),
      "application/json",
    );
  }

  /** Update node metadata (rename and/or ownership). */
  async patch(id: string, req: UpdateNodeRequest, recursiveOwnership?: boolean): Promise<Node> {
    if (!id.trim()) throw new Error("id is required");
    const q: Record<string, string> = {};
    if (recursiveOwnership !== undefined) q["recursiveOwnership"] = String(recursiveOwnership);
    return this.core.doJSON<Node>(
      "PATCH",
      `/v1/nodes/${encodeURIComponent(id)}`,
      q,
      JSON.stringify(req),
      "application/json",
    );
  }

  /** Delete a node and its subtree. */
  async delete(id: string): Promise<void> {
    if (!id.trim()) throw new Error("id is required");
    const endpoint = `/v1/nodes/${encodeURIComponent(id)}`;
    const resp = await this.core.doRaw("DELETE", endpoint);
    await ensureSuccess(resp, "DELETE", endpoint);
  }

  /** Get a thumbnail. Returns raw Response with image/jpeg body. */
  async thumbnailRaw(id: string, opts?: ThumbnailOptions): Promise<Response> {
    if (!id.trim()) throw new Error("id is required");
    const q: Record<string, string> = {};
    if (opts?.size) q["size"] = String(opts.size);
    const headers = opts?.ifNoneMatch ? { "If-None-Match": opts.ifNoneMatch } : undefined;
    return this.core.doRaw("GET", `/v1/nodes/${encodeURIComponent(id)}/thumbnail`, q, undefined, undefined, headers);
  }
}
