import { ClientCore } from "./core.js";
import { ensureSuccess } from "./errors.js";

/** Metadata for one captured version of a file. */
export interface VersionResponse {
  versionId: string;
  fileId: string;
  /** Capture time in unix milliseconds. */
  timestamp: number;
  size: number;
  mode: number;
  pinned: boolean;
  /** Opaque label set at snapshot/pin time. May contain JSON or plain text. */
  label?: string;
  /** Non-zero only after the source file was deleted; the version is
   * within the post-delete grace window before the pruner reclaims it. */
  deletedAt?: number;
}

/** One page of versions, oldest first. */
export interface ListVersionsResponse {
  items: VersionResponse[];
  /** Empty when this is the final page. */
  nextCursor?: string;
}

export interface VersionSnapshotRequest {
  label?: string;
}

export interface VersionPinRequest {
  /** null clears, undefined leaves unchanged, "" clears. */
  label?: string | null;
}

export interface VersionRestoreRequest {
  /** false (default) replaces source bytes in place after snapshotting
   * current state. true creates a fresh sibling file. */
  asNewFile?: boolean;
  /** Override default `<base>-restored<ext>` for as-new restores. */
  name?: string;
}

export interface VersionRestoreResponse {
  node: import("./types.js").Node;
  asNew: boolean;
}

export interface ListVersionsOptions {
  cursor?: string;
  limit?: number;
}

export interface RestoreOptions {
  asNewFile?: boolean;
  name?: string;
}

/**
 * Per-file version history. All endpoints respond 404 with
 * "versioning not supported on this mount" while versioning is disabled;
 * callers can use that as a capability check without a separate flag.
 */
export class VersionsClient {
  constructor(private readonly core: ClientCore) {}

  /** List one page of versions. */
  async list(fileId: string, opts?: ListVersionsOptions): Promise<ListVersionsResponse> {
    if (!fileId.trim()) throw new Error("fileId is required");
    const q: Record<string, string> = {};
    if (opts?.cursor) q["cursor"] = opts.cursor;
    if (opts?.limit && opts.limit > 0) q["limit"] = String(opts.limit);
    return this.core.doJSON<ListVersionsResponse>(
      "GET",
      `/v1/nodes/${encodeURIComponent(fileId)}/versions`,
      q,
    );
  }

  /** List every version, paging until the server has nothing more. */
  async listAll(fileId: string): Promise<VersionResponse[]> {
    const out: VersionResponse[] = [];
    let cursor: string | undefined;
    while (true) {
      const page = await this.list(fileId, { cursor });
      out.push(...page.items);
      if (!page.nextCursor) break;
      cursor = page.nextCursor;
    }
    return out;
  }

  /** Stream version bytes. Returns the raw Response unchanged. */
  async contentRaw(fileId: string, versionId: string): Promise<Response> {
    return this.core.doRaw("GET", this.versionEndpoint(fileId, versionId, "/content"));
  }

  /** Capture current bytes as a new pinned version, ignoring cooldown. */
  async snapshot(fileId: string, label?: string): Promise<VersionResponse> {
    if (!fileId.trim()) throw new Error("fileId is required");
    const body: VersionSnapshotRequest = label ? { label } : {};
    return this.core.doJSON<VersionResponse>(
      "POST",
      `/v1/nodes/${encodeURIComponent(fileId)}/versions/snapshot`,
      undefined,
      JSON.stringify(body),
      "application/json",
    );
  }

  /** Pin an existing version. Pass label=null to clear, omit to keep. */
  async pin(fileId: string, versionId: string, label?: string | null): Promise<VersionResponse> {
    const body: VersionPinRequest = {};
    if (label !== undefined) body.label = label;
    return this.core.doJSON<VersionResponse>(
      "POST",
      this.versionEndpoint(fileId, versionId, "/pin"),
      undefined,
      JSON.stringify(body),
      "application/json",
    );
  }

  /** Clear the pinned flag. Label is preserved. */
  async unpin(fileId: string, versionId: string): Promise<VersionResponse> {
    return this.core.doJSON<VersionResponse>(
      "POST",
      this.versionEndpoint(fileId, versionId, "/unpin"),
    );
  }

  /** Restore a version. Default = in-place; opts.asNewFile=true = sibling file. */
  async restore(
    fileId: string,
    versionId: string,
    opts?: RestoreOptions,
  ): Promise<VersionRestoreResponse> {
    const body: VersionRestoreRequest = {};
    if (opts?.asNewFile) body.asNewFile = true;
    if (opts?.name) body.name = opts.name;
    return this.core.doJSON<VersionRestoreResponse>(
      "POST",
      this.versionEndpoint(fileId, versionId, "/restore"),
      undefined,
      JSON.stringify(body),
      "application/json",
    );
  }

  /** Manual purge. Works on any version (incl. pinned). */
  async delete(fileId: string, versionId: string): Promise<void> {
    const endpoint = this.versionEndpoint(fileId, versionId, "");
    const resp = await this.core.doRaw("DELETE", endpoint);
    await ensureSuccess(resp, "DELETE", endpoint);
  }

  private versionEndpoint(fileId: string, versionId: string, suffix: string): string {
    if (!fileId.trim()) throw new Error("fileId is required");
    if (!versionId.trim()) throw new Error("versionId is required");
    return `/v1/nodes/${encodeURIComponent(fileId)}/versions/${encodeURIComponent(versionId)}${suffix}`;
  }
}
