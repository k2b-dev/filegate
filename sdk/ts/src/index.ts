import type { DirectURL, IndexStatus, Metadata, Node, Ownership, Page, RootInfo, SessionCreated, Stats, System, Version, VersionOptions, WriteOptions } from "./types.js";
import { checked, putDirect } from "./utils.js";
export * from "./types.js";
export { FilegateError } from "./utils.js";
export interface FilegateConfig { baseUrl: string; token: string; fetch?: typeof fetch }
/** Trusted backend client. Never expose the bearer token to an end-user browser. */
export class Filegate {
  readonly fetch: typeof fetch;
  constructor(private readonly config: FilegateConfig) {
    const url = new URL(config.baseUrl);
    if (!/^https?:$/.test(url.protocol) || url.username || url.password || url.search || url.hash || url.pathname !== "/") throw new TypeError("baseUrl must be an HTTP(S) origin");
    if (!config.token) throw new TypeError("token is required");
    this.fetch = config.fetch ?? globalThis.fetch;
  }
  root(name: string): RootClient { if (!/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$/.test(name)) throw new TypeError("Invalid root"); return new RootClient(this, name); }
  system(signal?: AbortSignal): Promise<System> { return this.json("GET", "/v1/system", undefined, undefined, signal); }
  roots(signal?: AbortSignal): Promise<RootInfo[]> { return this.json("GET", "/v1/roots", undefined, undefined, signal); }
  async raw(method: string, path: string, query?: Record<string, string | number | boolean | undefined>, body?: unknown, signal?: AbortSignal): Promise<Response> {
    const url = new URL(path, this.config.baseUrl);
    if (url.origin !== new URL(this.config.baseUrl).origin) throw new TypeError("Request must stay on Filegate origin");
    for (const [key, value] of Object.entries(query ?? {})) if (value !== undefined) url.searchParams.set(key, String(value));
    return this.fetch(url, { method, headers: { Authorization: `Bearer ${this.config.token}`, ...(body === undefined ? {} : { "Content-Type": "application/json" }) }, body: body === undefined ? undefined : JSON.stringify(body), signal, redirect: "error" });
  }
  async json<T>(method: string, path: string, query?: Record<string, string | number | boolean | undefined>, body?: unknown, signal?: AbortSignal): Promise<T> { return checked<T>(await this.raw(method, path, query, body, signal)); }
}
export class RootClient {
  private readonly prefix: string;
  constructor(private readonly client: Filegate, readonly name: string) { this.prefix = `/v1/roots/${encodeURIComponent(name)}`; }
  info(): Promise<RootInfo> { return this.client.json("GET", this.prefix); }
  stat(path: string): Promise<Node> { return this.client.json("GET", `${this.prefix}/stat`, { path }); }
  resolve(id: string): Promise<Node> { return this.client.json("GET", `${this.prefix}/resolve`, { id }); }
  list(path = ".", options: { after?: string; limit?: number } = {}): Promise<Page> { return this.client.json("GET", `${this.prefix}/entries`, { path, ...options }); }
  search(q: string, options: { path?: string; after?: string; limit?: number; maxEntries?: number; signal?: AbortSignal } = {}): Promise<Page> { const { signal, ...query } = options; return this.client.json("GET", `${this.prefix}/search`, { q, ...query }, undefined, signal); }
  contentRaw(path: string, signal?: AbortSignal): Promise<Response> { return this.client.raw("GET", `${this.prefix}/content`, { path }, undefined, signal); }
  thumbnailRaw(path: string, width = 256, height = 256): Promise<Response> { return this.client.raw("GET", `${this.prefix}/thumbnail`, { path, width, height }); }
  archiveRaw(path = ".", signal?: AbortSignal): Promise<Response> { return this.client.raw("GET", `${this.prefix}/archive`, { path }, undefined, signal); }
  setOwnership(path: string, ownership: Ownership): Promise<Node> { return this.client.json("PATCH", `${this.prefix}/ownership`, { path }, ownership); }
  mkdir(path: string, ownership?: Ownership): Promise<Node> { return this.client.json("POST", `${this.prefix}/directories`, undefined, { path, ownership }); }
  remove(path: string, recursive = false): Promise<void> { return this.client.json("DELETE", `${this.prefix}/files`, { path, recursive }); }
  transfer(path: string, targetRoot: string, targetPath: string, options: WriteOptions & { move?: boolean } = {}): Promise<Node> { return this.client.json("POST", `${this.prefix}/transfers`, undefined, { path, targetRoot, targetPath, ...options }); }
  directUpload(path: string, size: number, options: WriteOptions & { expiresIn?: number } = {}): Promise<DirectURL> { return this.client.json("POST", `${this.prefix}/uploads/direct`, undefined, { path, size, ...options }); }
  directDownload(path: string, expiresIn = 900): Promise<DirectURL> { return this.client.json("POST", `${this.prefix}/downloads/direct`, undefined, { path, expiresIn }); }
  createSession(path: string, size: number, options: WriteOptions = {}): Promise<SessionCreated> { return this.client.json("POST", `${this.prefix}/uploads/sessions`, undefined, { path, size, ...options }); }
  async put(path: string, body: Blob, options: WriteOptions = {}, signal?: AbortSignal): Promise<Node> { const capability = await this.directUpload(path, body.size, options); return putDirect(capability.url, body, { fetch: this.client.fetch, signal }); }
  rebuild(signal?: AbortSignal): Promise<IndexStatus> { return this.client.json("POST", `${this.prefix}/index/rebuild`, undefined, undefined, signal); }
  refreshStats(maxEntries = 100000, signal?: AbortSignal): Promise<Stats> { return this.client.json("POST", `${this.prefix}/stats/refresh`, { maxEntries }, undefined, signal); }
  versions(path: string): Promise<Version[]> { return this.client.json("GET", `${this.prefix}/versions`, { path }); }
  snapshot(path: string, options: VersionOptions = {}): Promise<Version> { return this.client.json("POST", `${this.prefix}/versions`, { path }, options); }
  updateVersion(path: string, id: string, options: { pinned: boolean; metadata?: Metadata }): Promise<Version> { return this.client.json("PATCH", `${this.prefix}/versions/${encodeURIComponent(id)}`, { path }, options); }
  deleteVersion(path: string, id: string): Promise<void> { return this.client.json("DELETE", `${this.prefix}/versions/${encodeURIComponent(id)}`, { path }); }
  restore(path: string, id: string): Promise<Node> { return this.client.json("POST", `${this.prefix}/versions/${encodeURIComponent(id)}/restore`, { path }); }
  versionContentRaw(path: string, id: string): Promise<Response> { return this.client.raw("GET", `${this.prefix}/versions/${encodeURIComponent(id)}/content`, { path }); }
  prune(): Promise<{ deleted: number }> { return this.client.json("POST", `${this.prefix}/versions/prune`); }
}
