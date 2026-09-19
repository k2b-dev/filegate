import type { TransferOptions, TransferResult, DownloadOptions, ListingOptions, ExecutionIdentity, ACL, ACLScope, ArchiveItem, ArchiveLease, DirectoryOptions, DirectURL, ThumbnailLeaseOptions, IndexStatus, Metadata, Node, Ownership, Page, RootInfo, Session, SessionCreated, SessionCreateOptions, SessionSegmentPage, SessionLease, SessionLeaseRequest, Stats, System, Version, VersionOptions, WriteOptions } from "./types.js";
import { archiveRaw, checked, putDirect, DirectSession } from "./utils.js";
export * from "./types.js";
export { FilegateError } from "./utils.js";
export interface FilegateConfig { baseUrl: string; token: string; fetch?: typeof fetch; transferBaseUrl?: string }
/** Trusted backend client. Never expose the bearer token to an end-user browser. */
export class Filegate {
  readonly fetch: typeof fetch;
  private execution?: string;
  constructor(private readonly config: FilegateConfig) {
    const url = new URL(config.baseUrl);
    if (!/^https?:$/.test(url.protocol) || url.username || url.password || url.search || url.hash || url.pathname !== "/") throw new TypeError("baseUrl must be an HTTP(S) origin");
    if (!config.token) throw new TypeError("token is required");
    if (config.transferBaseUrl !== undefined) {
      const transfer = new URL(config.transferBaseUrl);
      if (!/^https?:$/.test(transfer.protocol) || transfer.username || transfer.password || transfer.search || transfer.hash || transfer.pathname !== "/") throw new TypeError("transferBaseUrl must be an HTTP(S) origin");
    }
    this.fetch = (config.fetch ?? globalThis.fetch).bind(globalThis);
  }
  /** Return an independent backend client bound to Unix credentials. Administrative operations reject this identity. */
  as(identity: ExecutionIdentity): Filegate {
    const validID = (id: number): boolean => Number.isInteger(id) && id >= 0 && id < 0xffffffff;
    if (!validID(identity.uid) || identity.uid === 0 || !validID(identity.gid) || (identity.groups?.length ?? 0) > 64 || identity.groups?.some(id => !validID(id))) throw new TypeError("Invalid execution identity");
    const client = new Filegate(this.config);
    client.execution = JSON.stringify({ uid: identity.uid, gid: identity.gid, groups: [...new Set(identity.groups ?? [])].sort((a, b) => a - b) });
    return client;
  }
  root(name: string): RootClient { if (!/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$/.test(name)) throw new TypeError("Invalid root"); return new RootClient(this, name); }
  system(signal?: AbortSignal): Promise<System> { return this.json("GET", "/v1/system", undefined, undefined, signal); }
  roots(signal?: AbortSignal): Promise<RootInfo[]> { return this.json("GET", "/v1/roots", undefined, undefined, signal); }
  archiveLease(items: ArchiveItem[], expiresIn?: number): Promise<ArchiveLease> { return this.json("POST", "/v1/downloads/archives", undefined, { items, expiresIn }); }
  archiveRaw(lease: ArchiveLease, signal?: AbortSignal): Promise<Response> { return archiveRaw({ ...lease, url: this.transferUrl(lease.url) }, { fetch: this.fetch, signal }); }
  /** Map only a backend-issued Filegate lease to the explicitly trusted transfer origin. */
  transferUrl(leaseUrl: string): string {
    const url = new URL(leaseUrl);
    if (!/^https?:$/.test(url.protocol) || url.username || url.password || url.hash || !/^\/v1\/direct\/[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/.test(url.pathname)) throw new TypeError("Expected a signed Filegate transfer URL");
    if (this.config.transferBaseUrl === undefined) return leaseUrl;
    const origin = new URL(this.config.transferBaseUrl);
    url.protocol = origin.protocol; url.host = origin.host;
    return url.href;
  }
  /** Server-side direct reads preserve HTTP errors and send no backend token. */
  downloadRaw(lease: DirectURL, signal?: AbortSignal): Promise<Response> { return this.fetch(this.transferUrl(lease.url), { method: "GET", signal, credentials: "omit", redirect: "error" }); }
  directSession(lease: SessionLease): DirectSession { return new DirectSession(this.transferUrl(lease.url), this.fetch); }
  async raw(method: string, path: string, query?: Record<string, string | number | boolean | undefined>, body?: unknown, signal?: AbortSignal): Promise<Response> {
    const url = new URL(path, this.config.baseUrl);
    if (url.origin !== new URL(this.config.baseUrl).origin) throw new TypeError("Request must stay on Filegate origin");
    for (const [key, value] of Object.entries(query ?? {})) if (value !== undefined) url.searchParams.set(key, String(value));
    return this.fetch(url, { method, headers: { Authorization: `Bearer ${this.config.token}`, ...(this.execution === undefined ? {} : { "X-Filegate-Execution": this.execution }), ...(body === undefined ? {} : { "Content-Type": "application/json" }) }, body: body === undefined ? undefined : JSON.stringify(body), signal, redirect: "error" });
  }
  async json<T>(method: string, path: string, query?: Record<string, string | number | boolean | undefined>, body?: unknown, signal?: AbortSignal): Promise<T> { return checked<T>(await this.raw(method, path, query, body, signal)); }
}
export class RootClient {
  private readonly prefix: string;
  constructor(private readonly client: Filegate, readonly name: string) { this.prefix = `/v1/roots/${encodeURIComponent(name)}`; }
  /** Bind kernel-enforced Unix credentials without changing this root client. */
  as(identity: ExecutionIdentity): RootClient { return this.client.as(identity).root(this.name); }
  info(): Promise<RootInfo> { return this.client.json("GET", this.prefix); }
  stat(path: string): Promise<Node> { return this.client.json("GET", `${this.prefix}/stat`, { path }); }
  resolve(id: string): Promise<Node> { return this.client.json("GET", `${this.prefix}/resolve`, { id }); }
  list(path = ".", options: ListingOptions = {}): Promise<Page> { return this.client.json("GET", `${this.prefix}/entries`, { path, ...options }); }
  search(q: string, options: ListingOptions & { path?: string; signal?: AbortSignal } = {}): Promise<Page> { const { signal, ...query } = options; return this.client.json("GET", `${this.prefix}/search`, { q, ...query }, undefined, signal); }
  contentRaw(path: string, signal?: AbortSignal): Promise<Response> { return this.client.raw("GET", `${this.prefix}/content`, { path }, undefined, signal); }
  thumbnailRaw(path: string, width = 256, height = 256): Promise<Response> { return this.client.raw("GET", `${this.prefix}/thumbnail`, { path, width, height }); }
  setOwnership(path: string, ownership: Ownership): Promise<Node> { return this.client.json("PATCH", `${this.prefix}/ownership`, { path }, ownership); }
  /** Read an access or default ACL; absent default ACLs have empty entries. */
  getACL(path: string, scope: ACLScope): Promise<ACL> { return this.client.json("GET", `${this.prefix}/acl`, { path, scope }); }
  /** Replace one ACL scope. Named entries require an explicit mask. */
  setACL(path: string, scope: ACLScope, acl: ACL): Promise<ACL> { return this.client.json("PUT", `${this.prefix}/acl`, { path, scope }, acl); }
  /** Remove future-child inheritance without changing existing children. */
  clearDefaultACL(path: string): Promise<void> { return this.client.json("DELETE", `${this.prefix}/acl`, { path, scope: "default" }); }
  mkdir(path: string, options: DirectoryOptions = {}): Promise<Node> { return this.client.json("POST", `${this.prefix}/directories`, undefined, { path, ...options }); }
  remove(path: string, recursive = false): Promise<void> { return this.client.json("DELETE", `${this.prefix}/files`, { path, recursive }); }
  transfer(path: string, targetRoot: string, targetPath: string, options: TransferOptions = {}): Promise<TransferResult> { return this.client.json("POST", `${this.prefix}/transfers`, undefined, { path, targetRoot, targetPath, ...options }); }
  transferStatus(id: string): Promise<TransferResult> { return this.client.json("GET", `${this.prefix}/transfers/${encodeURIComponent(id)}`); }
  resumeTransfer(id: string): Promise<TransferResult> { return this.client.json("POST", `${this.prefix}/transfers/${encodeURIComponent(id)}/resume`); }
  abandonTransfer(id: string): Promise<TransferResult> { return this.client.json("POST", `${this.prefix}/transfers/${encodeURIComponent(id)}/abandon`); }
  copyVersion(path: string, id: string, targetRoot: string, targetPath: string, options: WriteOptions = {}): Promise<Node> { return this.client.json("POST", `${this.prefix}/versions/${encodeURIComponent(id)}/copy`, undefined, { path, targetRoot, targetPath, ...options }); }
  directUpload(path: string, size: number, options: WriteOptions & { expiresIn?: number } = {}): Promise<DirectURL> { return this.client.json("POST", `${this.prefix}/uploads/direct`, undefined, { path, size, ...options }); }
  directDownload(path: string, options: DownloadOptions = {}): Promise<DirectURL> { return this.client.json("POST", `${this.prefix}/downloads/direct`, undefined, { path, ...options }); }
  /** Issue a GET/HEAD lease for exactly this historical version. */
  directVersionDownload(path: string, id: string, options: DownloadOptions = {}): Promise<DirectURL> { return this.client.json("POST", `${this.prefix}/versions/${encodeURIComponent(id)}/downloads/direct`, undefined, { path, ...options }); }
  /** Issue a GET/HEAD lease with a fixed thumbnail size. */
  directThumbnail(path: string, options: ThumbnailLeaseOptions = {}): Promise<DirectURL> { return this.client.json("POST", `${this.prefix}/thumbnail/direct`, undefined, { ...options, path }); }
  createSession(path: string, size: number, options: SessionCreateOptions = {}): Promise<SessionCreated> { return this.client.json("POST", `${this.prefix}/uploads/sessions`, undefined, { path, size, ...options }); }
  session(id: string): Promise<Session> { return this.client.json("GET", `${this.prefix}/uploads/sessions/${encodeURIComponent(id)}`); }
  sessionSegments(id: string, after = -1, limit = 100): Promise<SessionSegmentPage> { return this.client.json("GET", `${this.prefix}/uploads/sessions/${encodeURIComponent(id)}/segments`, { after, limit }); }
  sessionLease(id: string, options: SessionLeaseRequest = {}): Promise<SessionLease> { return this.client.json("POST", `${this.prefix}/uploads/sessions/${encodeURIComponent(id)}/lease`, undefined, options); }
  commitSession(id: string): Promise<Node> { return this.client.json("POST", `${this.prefix}/uploads/sessions/${encodeURIComponent(id)}/commit`); }
  abortSession(id: string): Promise<void> { return this.client.json("DELETE", `${this.prefix}/uploads/sessions/${encodeURIComponent(id)}`); }
  async put(path: string, body: Blob, options: WriteOptions = {}, signal?: AbortSignal): Promise<Node> { const capability = await this.directUpload(path, body.size, options); return putDirect(this.client.transferUrl(capability.url), body, { fetch: this.client.fetch, signal }); }
  rebuild(signal?: AbortSignal): Promise<IndexStatus> { return this.client.json("POST", `${this.prefix}/index/rebuild`, undefined, undefined, signal); }
  stats(): Promise<Stats | null> { return this.client.json("GET", `${this.prefix}/stats`); }
  recursiveStats(path: string, maxEntries = 100000, signal?: AbortSignal): Promise<Stats> { return this.client.json("POST", `${this.prefix}/stats/refresh`, { path, maxEntries }, undefined, signal); }
  refreshStats(maxEntries = 100000, signal?: AbortSignal): Promise<Stats> { return this.client.json("POST", `${this.prefix}/stats/refresh`, { maxEntries }, undefined, signal); }
  versions(path: string): Promise<Version[]> { return this.client.json("GET", `${this.prefix}/versions`, { path }); }
  snapshot(path: string, options: VersionOptions = {}): Promise<Version> { return this.client.json("POST", `${this.prefix}/versions`, { path }, options); }
  updateVersion(path: string, id: string, options: { pinned: boolean; metadata?: Metadata }): Promise<Version> { return this.client.json("PATCH", `${this.prefix}/versions/${encodeURIComponent(id)}`, { path }, options); }
  deleteVersion(path: string, id: string): Promise<void> { return this.client.json("DELETE", `${this.prefix}/versions/${encodeURIComponent(id)}`, { path }); }
  restore(path: string, id: string): Promise<Node> { return this.client.json("POST", `${this.prefix}/versions/${encodeURIComponent(id)}/restore`, { path }); }
  versionContentRaw(path: string, id: string): Promise<Response> { return this.client.raw("GET", `${this.prefix}/versions/${encodeURIComponent(id)}/content`, { path }); }
  prune(): Promise<{ deleted: number }> { return this.client.json("POST", `${this.prefix}/versions/prune`); }
}
