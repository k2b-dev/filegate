import type { ArchiveLease, Node, SessionStatus, SessionSegmentPage } from "./types.js";
export interface Segment { index: number; offset: number; size: number }
export function segments(size: number, chunkSize = 8 * 1024 * 1024): Segment[] {
  if (!Number.isSafeInteger(size) || size < 0 || !Number.isSafeInteger(chunkSize) || chunkSize < 1) throw new RangeError("Invalid segment size");
  return Array.from({ length: Math.ceil(size / chunkSize) }, (_, index) => ({ index, offset: index * chunkSize, size: Math.min(chunkSize, size - index * chunkSize) }));
}
export async function sha256(data: BufferSource): Promise<string> {
  const hash = new Uint8Array(await crypto.subtle.digest("SHA-256", data));
  return `sha256:${Array.from(hash, b => b.toString(16).padStart(2, "0")).join("")}`;
}
export class FilegateError extends Error {
  constructor(readonly status: number, readonly code: string, message: string) { super(message); this.name = "FilegateError"; }
}
export async function checked<T>(response: Response): Promise<T> {
  if (!response.ok) {
    let code = "http_error", message = response.statusText;
    try { const value: unknown = await response.json(); if (typeof value === "object" && value !== null) { if ("error" in value && typeof value.error === "string") code = value.error; if ("message" in value && typeof value.message === "string") message = value.message; } } catch { /* A reverse proxy may return plain text. */ }
    throw new FilegateError(response.status, code, message);
  }
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}
export async function putDirect(url: string, body: BodyInit, options: { signal?: AbortSignal; fetch?: typeof fetch } = {}): Promise<Node> {
  return checked<Node>(await (options.fetch ?? fetch)(url, { method: "PUT", body, signal: options.signal, credentials: "omit", redirect: "error" }));
}
/** Only a scoped session URL is needed in a browser. No bearer token is sent. */
export class DirectSession {
  private readonly request: typeof fetch;
  constructor(readonly url: string, request?: typeof fetch) { this.request = (request ?? globalThis.fetch).bind(globalThis); }
  status(signal?: AbortSignal): Promise<SessionStatus> { return this.call("GET", undefined, undefined, signal); }
  segments(after = -1, limit = 100, signal?: AbortSignal): Promise<SessionSegmentPage> {
    if (!Number.isInteger(after) || after < -1 || after >= 10000 || !Number.isInteger(limit) || limit < 1 || limit > 1000) throw new RangeError("Invalid segment page");
    return this.call("GET", undefined, undefined, signal, { segments: "1", after: String(after), limit: String(limit) });
  }
  put(index: number, body: BodyInit, signal?: AbortSignal): Promise<SessionStatus> { return this.call("PUT", body, index, signal); }
  abort(signal?: AbortSignal): Promise<void> { return this.call("DELETE", undefined, undefined, signal); }
  private async call<T>(method: string, body?: BodyInit, segment?: number, signal?: AbortSignal, query?: Record<string, string>): Promise<T> {
    const url = new URL(this.url);
    if (segment !== undefined) { if (!Number.isInteger(segment) || segment < 0) throw new RangeError("Invalid segment"); url.searchParams.set("segment", String(segment)); }
    for (const [key, value] of Object.entries(query ?? {})) url.searchParams.set(key, value);
    const retryable = method === "GET" || method === "PUT" && (typeof body === "string" || body instanceof Blob || body instanceof ArrayBuffer || ArrayBuffer.isView(body));
    for (let attempt = 0; ; attempt++) {
      signal?.throwIfAborted();
      let response: Response;
      try { response = await this.request(url, { method, body, signal, credentials: "omit", redirect: "error" }); }
      catch (error) {
        if (!retryable || attempt >= 2 || signal?.aborted) throw error;
        await retryDelay(250 * 2 ** attempt, signal);
        continue;
      }
      if (retryable && attempt < 2 && [429, 502, 503, 504].includes(response.status)) {
        const requested = response.headers.get("Retry-After");
        const seconds = requested === null ? NaN : Number(requested);
        const hinted = Number.isFinite(seconds) && seconds >= 0 ? seconds * 1000 : requested === null ? NaN : Math.max(0, Date.parse(requested) - Date.now());
        // Never retry earlier than Retry-After or wait without a small bound.
        if (Number.isFinite(hinted) && hinted > 2000) return checked<T>(response);
        const delay = Number.isFinite(hinted) ? hinted : 250 * 2 ** attempt;
        await response.body?.cancel();
        await retryDelay(delay, signal);
        continue;
      }
      return checked<T>(response);
    }
  }
  async upload(blob: Blob, options: { signal?: AbortSignal; onProgress?: (bytes: number, total: number) => void } = {}): Promise<SessionStatus> {
    let current = await this.status(options.signal);
    if (current.state !== "open") return current;
    if (blob.size !== current.size) throw new RangeError("Upload size differs from session");
    const parts = segments(blob.size, current.chunkSize);
    if (parts.length > 10000) throw new RangeError("Too many session segments");
    if (parts.length === 0) return current;
    const received = new Set<number>();
    let after = -1;
    // Verify each acknowledged chunk once before sending any new bytes.
    for (;;) {
      const page = await this.segments(after, 1000, options.signal);
      let previous = after;
      for (const receipt of page.items) {
        const part = parts[receipt.index];
        if (!part || receipt.index <= previous || received.has(receipt.index)) throw new Error("Invalid session segment page");
        options.signal?.throwIfAborted();
        const hash = await sha256(await blob.slice(part.offset, part.offset + part.size).arrayBuffer());
        options.signal?.throwIfAborted();
        if (hash !== receipt.hash) throw new Error("File contents differ from the uploaded session segments");
        received.add(receipt.index);
        previous = receipt.index;
      }
      if (page.next === undefined) break;
      if (page.items.length === 0 || page.next !== page.items[page.items.length - 1].index || page.next <= after) throw new Error("Invalid session segment cursor");
      after = page.next;
    }
    options.onProgress?.(current.received, blob.size);
    for (const part of parts) {
      if (received.has(part.index)) continue;
      current = await this.put(part.index, blob.slice(part.offset, part.offset + part.size), options.signal);
      options.onProgress?.(current.received, blob.size);
    }
    return current;
  }
}

function retryDelay(milliseconds: number, signal?: AbortSignal): Promise<void> {
  signal?.throwIfAborted();
  return new Promise((resolve, reject) => {
    const abort = (): void => { clearTimeout(timer); signal?.removeEventListener("abort", abort); reject(signal?.reason); };
    const timer = setTimeout(() => { signal?.removeEventListener("abort", abort); resolve(); }, milliseconds);
    signal?.addEventListener("abort", abort, { once: true });
  });
}

/** Stream a signed ZIP response without a backend token or buffering the archive. */
export function archiveRaw(lease: ArchiveLease, options: { signal?: AbortSignal; fetch?: typeof fetch } = {}): Promise<Response> {
  return (options.fetch ?? fetch)(lease.url, { method: "POST", body: new URLSearchParams({ manifest: lease.manifest }), signal: options.signal, credentials: "omit", redirect: "error" });
}
/** Start a native browser download; the browser streams the archive to disk. */
export function downloadArchive(lease: ArchiveLease): void {
  const url = new URL(lease.url);
  if (url.protocol !== "https:" && url.protocol !== "http:") throw new TypeError("Archive URL must use HTTP(S)");
  if (url.username || url.password) throw new TypeError("Archive URL must not contain credentials");
  const form = document.createElement("form");
  form.method = "POST";
  form.action = url.href;
  form.enctype = "application/x-www-form-urlencoded";
  const field = document.createElement("input");
  field.type = "hidden";
  field.name = "manifest";
  field.value = lease.manifest;
  form.append(field);
  document.body.append(form);
  try { form.submit(); } finally { form.remove(); }
}
