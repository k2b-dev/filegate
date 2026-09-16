import type { Node, Session } from "./types.js";
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
  return checked<Node>(await (options.fetch ?? fetch)(url, { method: "PUT", body, signal: options.signal, redirect: "error" }));
}
/** Only a scoped session URL is needed in a browser. No bearer token is sent. */
export class DirectSession {
  constructor(readonly url: string, private readonly request: typeof fetch = fetch) {}
  status(signal?: AbortSignal): Promise<Session> { return this.call("GET", undefined, undefined, signal); }
  put(index: number, body: BodyInit, signal?: AbortSignal): Promise<Session> { return this.call("PUT", body, index, signal); }
  commit(signal?: AbortSignal): Promise<Node> { return this.call("POST", undefined, undefined, signal); }
  abort(signal?: AbortSignal): Promise<void> { return this.call("DELETE", undefined, undefined, signal); }
  private async call<T>(method: string, body?: BodyInit, segment?: number, signal?: AbortSignal): Promise<T> {
    const url = new URL(this.url);
    if (segment !== undefined) { if (!Number.isInteger(segment) || segment < 0) throw new RangeError("Invalid segment"); url.searchParams.set("segment", String(segment)); }
    return checked<T>(await this.request(url, { method, body, signal, redirect: "error" }));
  }
  async upload(blob: Blob, options: { signal?: AbortSignal; onProgress?: (bytes: number, total: number) => void } = {}): Promise<Node> {
    const current = await this.status(options.signal);
    if (current.result) return current.result;
    if (blob.size !== current.size) throw new RangeError("Upload size differs from session");
    let received = current.received;
    for (const part of segments(blob.size, current.chunkSize)) {
      if (current.segments[String(part.index)]) {
 const hash = await sha256(await blob.slice(part.offset, part.offset + part.size).arrayBuffer());
 if (hash !== current.segments[String(part.index)]) throw new Error("File contents differ from the uploaded session segments");
 continue;
 }
      await this.put(part.index, blob.slice(part.offset, part.offset + part.size), options.signal);
      received += part.size; options.onProgress?.(received, blob.size);
    }
    return this.commit(options.signal);
  }
}
