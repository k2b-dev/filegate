import { ClientCore, type FetchImpl } from "./core.js";
import { ensureSuccess } from "./errors.js";
import type {
  DirectUploadURLRequest,
  DirectUploadURLResponse,
  Node,
  UploadSegmentResponse,
  UploadSessionBatchCreateRequest,
  UploadSessionBatchCreateResponse,
  UploadSessionCommitResponse,
  UploadSessionCreateRequest,
  UploadSessionDirect,
  UploadSessionResponse,
} from "./types.js";

export interface UploadSessionStatusRequest {
  sessionId: string;
}

export interface UploadSessionPutSegmentRequest {
  sessionId: string;
  index: number;
  body: BodyInit;
  checksum?: string;
  contentType?: string;
}

export interface UploadSessionCommitRequest {
  sessionId: string;
}

export interface UploadSessionAbortRequest {
  sessionId: string;
}

export class UploadSessionsClient {
  readonly segments = {
    put: (req: UploadSessionPutSegmentRequest): Promise<UploadSegmentResponse> =>
      this.putSegment(req),
    putRaw: (req: UploadSessionPutSegmentRequest): Promise<Response> =>
      this.putSegmentRaw(req),
  } as const;

  constructor(private readonly core: ClientCore) {}

  /** Create one resumable upload session for one file. */
  async create(req: UploadSessionCreateRequest): Promise<UploadSessionResponse> {
    return this.core.doJSON<UploadSessionResponse>(
      "POST",
      "/v1/uploads/sessions",
      undefined,
      JSON.stringify(req),
      "application/json",
    );
  }

  /** Create independent upload sessions in one request. */
  async createBatch(req: UploadSessionBatchCreateRequest): Promise<UploadSessionBatchCreateResponse> {
    return this.core.doJSON<UploadSessionBatchCreateResponse>(
      "POST",
      "/v1/uploads/sessions:batch",
      undefined,
      JSON.stringify(req),
      "application/json",
    );
  }

  /** Get current session status. */
  async status(req: UploadSessionStatusRequest): Promise<UploadSessionResponse> {
    const sessionId = requireSessionId(req.sessionId);
    return this.core.doJSON<UploadSessionResponse>(
      "GET",
      `/v1/uploads/sessions/${encodeURIComponent(sessionId)}`,
    );
  }

  /** Upload one segment and throw on non-2xx responses. */
  private async putSegment(req: UploadSessionPutSegmentRequest): Promise<UploadSegmentResponse> {
    const sessionId = requireSessionId(req.sessionId);
    const resp = await this.putSegmentRaw(req);
    await ensureSuccess(resp, "PUT", `/v1/uploads/sessions/${sessionId}/segments/${req.index}`);
    return (await resp.json()) as UploadSegmentResponse;
  }

  /** Upload one segment and return the raw Response for relay/proxy handlers. */
  private async putSegmentRaw(req: UploadSessionPutSegmentRequest): Promise<Response> {
    const sessionId = requireSessionId(req.sessionId);
    if (req.index < 0) throw new Error("index must be >= 0");
    const endpoint = `/v1/uploads/sessions/${encodeURIComponent(sessionId)}/segments/${req.index}`;
    const extra: Record<string, string> = {};
    if (req.checksum?.trim()) extra["X-Segment-Checksum"] = req.checksum.trim();

    return this.core.doRaw("PUT", endpoint, undefined, req.body, req.contentType ?? "application/octet-stream", extra);
  }

  /** Commit a complete session and return the created/updated node. */
  async commit(req: UploadSessionCommitRequest): Promise<UploadSessionCommitResponse> {
    const sessionId = requireSessionId(req.sessionId);
    return this.core.doJSON<UploadSessionCommitResponse>(
      "POST",
      `/v1/uploads/sessions/${encodeURIComponent(sessionId)}/commit`,
    );
  }

  /** Abort an in-progress session. */
  async abort(req: UploadSessionAbortRequest): Promise<void> {
    const sessionId = requireSessionId(req.sessionId);
    const resp = await this.core.doRaw("DELETE", `/v1/uploads/sessions/${encodeURIComponent(sessionId)}`);
    await ensureSuccess(resp, "DELETE", `/v1/uploads/sessions/${sessionId}`);
  }
}

export class UploadsClient {
  readonly sessions: UploadSessionsClient;

  constructor(private readonly core: ClientCore) {
    this.sessions = new UploadSessionsClient(core);
  }

  /** Mint a short-lived direct PUT URL for a browser/client upload. */
  async createDirectUploadURL(req: DirectUploadURLRequest): Promise<DirectUploadURLResponse> {
    return this.core.doJSON<DirectUploadURLResponse>(
      "POST",
      "/v1/uploads/direct",
      undefined,
      JSON.stringify(req),
      "application/json",
    );
  }
}

function requireSessionId(sessionId: string): string {
  const trimmed = sessionId.trim();
  if (!trimmed) throw new Error("sessionId is required");
  return trimmed;
}

export interface DirectUploadResult {
  node: Node;
  nodeId: string;
  createdId: string;
  statusCode: number;
  headers: Headers;
}

export type DirectUploadFinish =
  | { ok: true; result: DirectUploadResult }
  | { ok: false; error: unknown };

export interface DirectUploadOptions {
  fetchImpl?: FetchImpl;
  contentType?: string;
  signal?: AbortSignal;
  onSuccess?: (result: DirectUploadResult) => void | Promise<void>;
  onError?: (error: unknown) => void | Promise<void>;
  onFinish?: (outcome: DirectUploadFinish) => void | Promise<void>;
}

export interface DirectUploadSessionRequest {
  direct: UploadSessionDirect;
}

export interface DirectUploadSegmentRequest extends DirectUploadSessionRequest {
  index: number;
  body: BodyInit;
  checksum?: string;
  contentType?: string;
  fetchImpl?: FetchImpl;
  signal?: AbortSignal;
}

async function directFetch(opts?: { fetchImpl?: FetchImpl }): Promise<FetchImpl> {
  const fetchImpl = opts?.fetchImpl ?? globalThis.fetch?.bind(globalThis);
  if (!fetchImpl) throw new Error("fetch is not available");
  return fetchImpl;
}

async function directJSON<T>(
  direct: UploadSessionDirect,
  method: string,
  suffix: string,
  opts?: { fetchImpl?: FetchImpl; signal?: AbortSignal },
): Promise<T> {
  const fetchImpl = await directFetch(opts);
  const url = `${direct.baseUrl.replace(/\/+$/, "")}${suffix}`;
  const resp = await fetchImpl(url, {
    method,
    headers: { "Filegate-Upload-Session": direct.token },
    signal: opts?.signal,
  });
  await ensureSuccess(resp, method, url);
  if (resp.status === 204) return undefined as T;
  return (await resp.json()) as T;
}

async function putDirectSegment(req: DirectUploadSegmentRequest): Promise<UploadSegmentResponse> {
  if (req.index < 0) throw new Error("index must be >= 0");
  const fetchImpl = await directFetch(req);
  const url = `${req.direct.baseUrl.replace(/\/+$/, "")}/segments/${req.index}`;
  const headers: Record<string, string> = {
    "Filegate-Upload-Session": req.direct.token,
    "Content-Type": req.contentType ?? bodyContentType(req.body) ?? "application/octet-stream",
  };
  if (req.checksum?.trim()) headers["X-Segment-Checksum"] = req.checksum.trim();
  const resp = await fetchImpl(url, {
    method: "PUT",
    headers,
    body: req.body,
    signal: req.signal,
  });
  await ensureSuccess(resp, "PUT", url);
  return (await resp.json()) as UploadSegmentResponse;
}

async function status(req: DirectUploadSessionRequest & { fetchImpl?: FetchImpl; signal?: AbortSignal }): Promise<UploadSessionResponse> {
  return directJSON<UploadSessionResponse>(req.direct, "GET", "", req);
}

async function commit(req: DirectUploadSessionRequest & { fetchImpl?: FetchImpl; signal?: AbortSignal }): Promise<UploadSessionCommitResponse> {
  return directJSON<UploadSessionCommitResponse>(req.direct, "POST", "/commit", req);
}

async function abort(req: DirectUploadSessionRequest & { fetchImpl?: FetchImpl; signal?: AbortSignal }): Promise<void> {
  await directJSON<void>(req.direct, "DELETE", "", req);
}

export const directUploads = {
  segments: { put: putDirectSegment },
  status,
  commit,
  abort,
} as const;

export interface BrowserUploadFile {
  name: string;
  size: number;
  type?: string;
  webkitRelativePath?: string;
  slice(start?: number, end?: number): Blob;
}

export interface BrowserUploadSegment {
  index: number;
  offset: number;
  size: number;
  checksum: string;
}

export interface BrowserUploadAllowItem {
  id: string;
  kind: "direct" | "session";
  path: string;
  name: string;
  size: number;
  checksum?: string;
  segmentSize?: number;
  contentType?: string;
  segments?: BrowserUploadSegment[];
}

export type BrowserUploadDirective =
  | { kind: "direct"; direct: DirectUploadURLResponse }
  | { kind: "session"; session: UploadSessionResponse };

export type BrowserUploadConflictMode = "skip-existing" | "skip-identical" | "error" | "overwrite" | "rename";

export type BrowserUploadAllowResult =
  | { id: string; ok: true; upload?: BrowserUploadDirective; session?: UploadSessionResponse; node?: Node }
  | { id: string; ok: false; error: string; code?: string; skipped?: boolean; node?: Node };

export interface BrowserUploadAllowRequest {
  uploads: BrowserUploadAllowItem[];
  onConflict: BrowserUploadConflictMode;
}

export interface BrowserUploadAllowResponse {
  uploads: BrowserUploadAllowResult[];
}

export interface BrowserUploadConfig {
  segmentSize?: number;
  chunkSize?: number;
  /**
   * Files at or below this size go through one PUT instead of a session.
   *
   * Defaults to `segmentSize`: a file that would have been a single segment
   * gains nothing from a session, which costs three requests instead of one and
   * whose resumability amounts to retrying the same lone segment. Pass `0` to
   * send everything through sessions.
   *
   * Ignored when `onConflict` is `skip-identical`, which needs the checksum a
   * session computes.
   */
  directThresholdBytes?: number;
  onConflict?: BrowserUploadConflictMode;
  concurrency?: {
    hash?: number;
    files?: number;
    segments?: number;
  };
  batch?: {
    size: number;
    flushMs: number;
  };
}

export type BrowserUploadEvent =
  | { type: "start"; files: number; bytes: number }
  | { type: "file:hashing"; id: string; path: string; loaded: number; total: number }
  | { type: "file:allowed"; id: string; path: string }
  | { type: "file:skipped"; id: string; path: string; reason: string; code?: string; node?: Node }
  | { type: "file:rejected"; id: string; path: string; error: string; code?: string }
  | { type: "file:uploading"; id: string; path: string; loaded: number; total: number }
  | { type: "file:committing"; id: string; path: string }
  | { type: "file:done"; id: string; path: string; node?: Node }
  | { type: "file:error"; id: string; path: string; error: unknown }
  | { type: "stats"; transferred: number; total: number; elapsedMs: number; bytesPerSecond: number; remainingMs?: number };

export interface BrowserUploadRequest {
  files: Iterable<BrowserUploadFile>;
  path: string;
  allow(req: BrowserUploadAllowRequest): Promise<BrowserUploadAllowResponse>;
  config?: BrowserUploadConfig;
  fetchImpl?: FetchImpl;
  signal?: AbortSignal;
  onEvent?: (event: BrowserUploadEvent) => void;
}

export interface BrowserUploadResult {
  total: number;
  done: number;
  failed: number;
  rejected: number;
  skipped: number;
}

type UploadWork = {
  id: string;
  file: BrowserUploadFile;
  name: string;
  path: string;
  contentType?: string;
};

type HashedUploadWork = UploadWork & {
  kind: "session";
  checksum: string;
  segments: BrowserUploadSegment[];
};

type PlannedUploadWork =
  | (UploadWork & { kind: "direct" })
  | HashedUploadWork;

const EMPTY_SHA256 = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
const SHA256_K = [
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
] as const;

/** Upload browser files through Filegate direct upload sessions. */
export async function upload(req: BrowserUploadRequest): Promise<BrowserUploadResult> {
  const segmentSize = req.config?.segmentSize ?? 8 * 1024 * 1024;
  const chunkSize = req.config?.chunkSize ?? 4 * 1024 * 1024;
  const directThresholdBytes = req.config?.directThresholdBytes ?? segmentSize;
  const hashLimit = req.config?.concurrency?.hash ?? 2;
  const fileLimit = req.config?.concurrency?.files ?? 6;
  const segmentLimit = req.config?.concurrency?.segments ?? 6;
  const batch = req.config?.batch ?? { size: 32, flushMs: 20 };
  const onConflict = req.config?.onConflict ?? "skip-existing";
  if (segmentSize <= 0) throw new Error("segmentSize must be > 0");
  if (chunkSize <= 0) throw new Error("chunkSize must be > 0");
  if (batch.size <= 0) throw new Error("batch.size must be > 0");
  if (batch.flushMs < 0) throw new Error("batch.flushMs must be >= 0");

  const works = Array.from(req.files, (file, index) => {
    const name = file.webkitRelativePath || file.name || `file-${index + 1}`;
    return {
      id: `u${index + 1}`,
      file,
      name,
      path: cleanUploadPath(req.path, name),
      contentType: file.type || undefined,
    } satisfies UploadWork;
  });
  const result: BrowserUploadResult = { total: works.length, done: 0, failed: 0, rejected: 0, skipped: 0 };
  const totalBytes = works.reduce((sum, work) => sum + work.file.size, 0);
  const started = Date.now();
  let uploadStarted = 0;
  let transferred = 0;
  const uploadLimit = limit(fileLimit);
  const segmentUploadLimit = limit(segmentLimit);
  const uploadTasks: Promise<void>[] = [];
  const allowTasks: Promise<void>[] = [];
  let timer: ReturnType<typeof setTimeout> | undefined;
  let pending: PlannedUploadWork[] = [];

  emit(req, { type: "start", files: works.length, bytes: totalBytes });
  emitStats(req, started, uploadStarted, transferred, totalBytes);

  function countDone(kind: "done" | "failed" | "rejected" | "skipped") {
    result[kind]++;
  }

  function startUpload(work: PlannedUploadWork, allowed: Extract<BrowserUploadAllowResult, { ok: true }>) {
    const task = uploadLimit(async () => {
      try {
        if (work.file.size === 0) {
          emit(req, { type: "file:done", id: work.id, path: work.path, node: allowed.node });
          countDone("done");
          return;
        }
        const directive = allowed.upload ?? (allowed.session ? { kind: "session" as const, session: allowed.session } : undefined);
        if (!directive) throw new Error("upload directive missing");
        if (directive.kind === "direct") {
          if (!uploadStarted) uploadStarted = Date.now();
          emit(req, { type: "file:uploading", id: work.id, path: work.path, loaded: 0, total: work.file.size });
          const direct = await uploadDirect(directive.direct.uploadUrl, work.file.slice(0, work.file.size), {
            contentType: work.contentType || "application/octet-stream",
            fetchImpl: req.fetchImpl,
            signal: req.signal,
          });
          transferred += work.file.size;
          emit(req, { type: "file:uploading", id: work.id, path: work.path, loaded: work.file.size, total: work.file.size });
          emitStats(req, started, uploadStarted, transferred, totalBytes);
          emit(req, { type: "file:done", id: work.id, path: work.path, node: direct.node });
          countDone("done");
          return;
        }
        if (work.kind !== "session") throw new Error("session upload requires hashed work");
        const session = directive.session;
        if (!session.direct) throw new Error("direct upload session missing");
        await uploadSessionFile(req, work, session, segmentUploadLimit, (bytes, loaded) => {
          if (!uploadStarted && bytes > 0) uploadStarted = Date.now();
          transferred += bytes;
          emit(req, { type: "file:uploading", id: work.id, path: work.path, loaded, total: work.file.size });
          emitStats(req, started, uploadStarted, transferred, totalBytes);
        });
        emit(req, { type: "file:committing", id: work.id, path: work.path });
        const committed = await directUploads.commit({ direct: session.direct, fetchImpl: req.fetchImpl, signal: req.signal });
        emit(req, { type: "file:done", id: work.id, path: work.path, node: committed.node });
        countDone("done");
      } catch (error) {
        emit(req, { type: "file:error", id: work.id, path: work.path, error });
        countDone("failed");
      }
    });
    uploadTasks.push(task);
  }

  async function flush() {
    if (timer) {
      clearTimeout(timer);
      timer = undefined;
    }
    const batchItems = pending;
    pending = [];
    if (!batchItems.length) return;
    const task = (async () => {
      let out: BrowserUploadAllowResponse;
      try {
        out = await req.allow({
          onConflict,
          uploads: batchItems.map((work) => ({
            id: work.id,
            kind: work.kind,
            path: work.path,
            name: work.name,
            size: work.file.size,
            checksum: work.kind === "session" ? work.checksum : undefined,
            segmentSize: work.kind === "session" ? segmentSize : undefined,
            contentType: work.contentType,
            segments: work.kind === "session" ? work.segments : undefined,
          })),
        });
      } catch (error) {
        for (const work of batchItems) {
          emit(req, { type: "file:error", id: work.id, path: work.path, error });
          countDone("failed");
        }
        return;
      }
      const byID = new Map(out.uploads.map((item) => [item.id, item]));
      for (const work of batchItems) {
        const allowed = byID.get(work.id);
        if (!allowed) {
          emit(req, { type: "file:error", id: work.id, path: work.path, error: new Error("allow response missing") });
          countDone("failed");
          continue;
        }
        if (!allowed.ok) {
          if (allowed.skipped) {
            emit(req, {
              type: "file:skipped",
              id: work.id,
              path: work.path,
              reason: allowed.error,
              code: allowed.code,
              node: allowed.node,
            });
            countDone("skipped");
            continue;
          }
          emit(req, { type: "file:rejected", id: work.id, path: work.path, error: allowed.error, code: allowed.code });
          countDone("rejected");
          continue;
        }
        emit(req, { type: "file:allowed", id: work.id, path: work.path });
        startUpload(work, allowed);
      }
    })();
    allowTasks.push(task);
    await task;
  }

  function enqueueAllow(work: PlannedUploadWork) {
    pending.push(work);
    if (pending.length >= batch.size) {
      void flush();
      return;
    }
    if (!timer) timer = setTimeout(() => void flush(), batch.flushMs);
  }

  const sessionWorks: UploadWork[] = [];
  for (const work of works) {
    if (onConflict !== "skip-identical" && directThresholdBytes > 0 && work.file.size > 0 && work.file.size <= directThresholdBytes) {
      enqueueAllow({ ...work, kind: "direct" });
    } else {
      sessionWorks.push(work);
    }
  }

  await runLimited(sessionWorks, hashLimit, async (work) => {
    try {
      enqueueAllow(await hashUploadWork(req, work, segmentSize, chunkSize));
    } catch (error) {
      emit(req, { type: "file:error", id: work.id, path: work.path, error });
      countDone("failed");
    }
  });
  await flush();
  await Promise.all(allowTasks);
  await Promise.all(uploadTasks);
  emitStats(req, started, uploadStarted, transferred, totalBytes);
  return result;
}

async function hashUploadWork(
  req: BrowserUploadRequest,
  work: UploadWork,
  segmentSize: number,
  chunkSize: number,
): Promise<HashedUploadWork> {
  if (work.file.size === 0) {
    emit(req, { type: "file:hashing", id: work.id, path: work.path, loaded: 0, total: 0 });
    return { ...work, kind: "session", checksum: EMPTY_SHA256, segments: [] };
  }

  const fileHash = sha256();
  const segments: BrowserUploadSegment[] = [];
  let offset = 0;
  while (offset < work.file.size) {
    const segmentEnd = Math.min(work.file.size, offset + segmentSize);
    const segmentHash = sha256();
    let pos = offset;
    while (pos < segmentEnd) {
      if (req.signal?.aborted) throw req.signal.reason ?? new Error("upload aborted");
      const end = Math.min(segmentEnd, pos + chunkSize);
      const bytes = new Uint8Array(await work.file.slice(pos, end).arrayBuffer());
      fileHash.update(bytes);
      segmentHash.update(bytes);
      pos = end;
      emit(req, { type: "file:hashing", id: work.id, path: work.path, loaded: pos, total: work.file.size });
    }
    segments.push({
      index: segments.length,
      offset,
      size: segmentEnd - offset,
      checksum: `sha256:${segmentHash.hex()}`,
    });
    offset = segmentEnd;
  }
  return { ...work, kind: "session", checksum: `sha256:${fileHash.hex()}`, segments };
}

async function uploadSessionFile(
  req: BrowserUploadRequest,
  work: HashedUploadWork,
  session: UploadSessionResponse,
  segmentUploadLimit: ReturnType<typeof limit>,
  onSegment: (bytes: number, loaded: number) => void,
) {
  const uploaded = new Set(session.uploadedSegments || []);
  let loaded = uploaded.size ? session.segments.filter((segment) => uploaded.has(segment.index)).reduce((sum, segment) => sum + segment.size, 0) : 0;
  await Promise.all(
    work.segments.filter((segment) => !uploaded.has(segment.index)).map((segment) =>
      segmentUploadLimit(async () => {
        if (!session.direct) throw new Error("direct upload session missing");
        await directUploads.segments.put({
          direct: session.direct,
          index: segment.index,
          body: work.file.slice(segment.offset, segment.offset + segment.size),
          checksum: segment.checksum,
          contentType: work.contentType || "application/octet-stream",
          fetchImpl: req.fetchImpl,
          signal: req.signal,
        });
        loaded += segment.size;
        onSegment(segment.size, loaded);
      }),
    ),
  );
}

function emit(req: BrowserUploadRequest, event: BrowserUploadEvent) {
  try {
    req.onEvent?.(event);
  } catch {
    // Progress observers must not change upload semantics.
  }
}

function emitStats(req: BrowserUploadRequest, started: number, uploadStarted: number, transferred: number, total: number) {
  const elapsedMs = Math.max(1, Date.now() - started);
  const uploadElapsedMs = uploadStarted ? Math.max(1, Date.now() - uploadStarted) : elapsedMs;
  const bytesPerSecond = transferred / (uploadElapsedMs / 1000);
  emit(req, {
    type: "stats",
    transferred,
    total,
    elapsedMs,
    bytesPerSecond,
    remainingMs: bytesPerSecond > 0 ? ((total - transferred) / bytesPerSecond) * 1000 : undefined,
  });
}

function cleanUploadPath(parent: string, name: string) {
  const p = parent.replace(/^\/+|\/+$/g, "");
  const n = name.replace(/^\/+|\/+$/g, "");
  return p ? `${p}/${n}` : n;
}

function limit(concurrency: number) {
  if (concurrency <= 0) throw new Error("concurrency must be > 0");
  let active = 0;
  const queue: Array<() => void> = [];
  return async <T>(fn: () => Promise<T>): Promise<T> =>
    new Promise<T>((resolve, reject) => {
      const run = () => {
        active++;
        fn().then(resolve, reject).finally(() => {
          active--;
          queue.shift()?.();
        });
      };
      active < concurrency ? run() : queue.push(run);
    });
}

async function runLimited<T>(items: T[], concurrency: number, fn: (item: T, index: number) => Promise<void>) {
  const run = limit(concurrency);
  await Promise.all(items.map((item, index) => run(() => fn(item, index))));
}

function sha256() {
  let h0 = 0x6a09e667;
  let h1 = 0xbb67ae85;
  let h2 = 0x3c6ef372;
  let h3 = 0xa54ff53a;
  let h4 = 0x510e527f;
  let h5 = 0x9b05688c;
  let h6 = 0x1f83d9ab;
  let h7 = 0x5be0cd19;
  const buf = new Uint8Array(64);
  const w = new Int32Array(64);
  let bufLen = 0;
  let total = 0;

  function rotr(x: number, n: number) {
    return (x >>> n) | (x << (32 - n));
  }

  function block(p: Uint8Array, off: number) {
    for (let i = 0; i < 16; i++) {
      w[i] = (p[off + i * 4] << 24) | (p[off + i * 4 + 1] << 16) | (p[off + i * 4 + 2] << 8) | p[off + i * 4 + 3];
    }
    for (let i = 16; i < 64; i++) {
      const a = w[i - 15];
      const b = w[i - 2];
      w[i] = (w[i - 16] + (rotr(a, 7) ^ rotr(a, 18) ^ (a >>> 3)) + w[i - 7] + (rotr(b, 17) ^ rotr(b, 19) ^ (b >>> 10))) | 0;
    }
    let a = h0;
    let b = h1;
    let c = h2;
    let d = h3;
    let e = h4;
    let f = h5;
    let g = h6;
    let h = h7;
    for (let i = 0; i < 64; i++) {
      const t1 = (h + (rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25)) + ((e & f) ^ (~e & g)) + SHA256_K[i] + w[i]) | 0;
      const t2 = ((rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22)) + ((a & b) ^ (a & c) ^ (b & c))) | 0;
      h = g;
      g = f;
      f = e;
      e = (d + t1) | 0;
      d = c;
      c = b;
      b = a;
      a = (t1 + t2) | 0;
    }
    h0 = (h0 + a) | 0;
    h1 = (h1 + b) | 0;
    h2 = (h2 + c) | 0;
    h3 = (h3 + d) | 0;
    h4 = (h4 + e) | 0;
    h5 = (h5 + f) | 0;
    h6 = (h6 + g) | 0;
    h7 = (h7 + h) | 0;
  }

  return {
    update(data: Uint8Array) {
      total += data.length;
      let i = 0;
      if (bufLen) {
        while (bufLen < 64 && i < data.length) buf[bufLen++] = data[i++];
        if (bufLen === 64) {
          block(buf, 0);
          bufLen = 0;
        }
      }
      while (i + 64 <= data.length) {
        block(data, i);
        i += 64;
      }
      while (i < data.length) buf[bufLen++] = data[i++];
    },
    hex() {
      const bits = total * 8;
      buf[bufLen++] = 0x80;
      if (bufLen > 56) {
        while (bufLen < 64) buf[bufLen++] = 0;
        block(buf, 0);
        bufLen = 0;
      }
      while (bufLen < 56) buf[bufLen++] = 0;
      const hi = Math.floor(bits / 4294967296);
      const lo = bits >>> 0;
      buf[56] = (hi >>> 24) & 255;
      buf[57] = (hi >>> 16) & 255;
      buf[58] = (hi >>> 8) & 255;
      buf[59] = hi & 255;
      buf[60] = (lo >>> 24) & 255;
      buf[61] = (lo >>> 16) & 255;
      buf[62] = (lo >>> 8) & 255;
      buf[63] = lo & 255;
      block(buf, 0);
      return [h0, h1, h2, h3, h4, h5, h6, h7].map((x) => (`00000000${(x >>> 0).toString(16)}`).slice(-8)).join("");
    },
  };
}

/** Upload to a presigned Filegate direct-upload URL without a REST bearer token. */
export async function uploadDirect(
  uploadUrl: string,
  data: BodyInit,
  opts?: DirectUploadOptions,
): Promise<DirectUploadResult> {
  const fetchImpl = opts?.fetchImpl ?? globalThis.fetch?.bind(globalThis);
  if (!fetchImpl) throw new Error("fetch is not available");
  if (!uploadUrl.trim()) throw new Error("uploadUrl is required");

  let result: DirectUploadResult | undefined;
  let caught: unknown;
  try {
    const headers: Record<string, string> = {};
    const contentType = opts?.contentType ?? bodyContentType(data);
    if (contentType) headers["Content-Type"] = contentType;

    const resp = await fetchImpl(uploadUrl, {
      method: "PUT",
      headers,
      body: data,
      signal: opts?.signal,
    });
    await ensureSuccess(resp, "PUT", uploadUrl);
    const node = (await resp.json()) as Node;
    result = {
      node,
      nodeId: resp.headers.get("X-Node-Id") ?? "",
      createdId: resp.headers.get("X-Created-Id") ?? "",
      statusCode: resp.status,
      headers: resp.headers,
    };
    await opts?.onSuccess?.(result);
    return result;
  } catch (err) {
    caught = err;
    await opts?.onError?.(err);
    throw err;
  } finally {
    if (result) {
      await opts?.onFinish?.({ ok: true, result });
    } else {
      await opts?.onFinish?.({ ok: false, error: caught });
    }
  }
}

function bodyContentType(data: BodyInit): string | undefined {
  const maybeTyped = data as { type?: unknown };
  if (typeof maybeTyped.type === "string" && maybeTyped.type.trim()) {
    return maybeTyped.type;
  }
  return undefined;
}
