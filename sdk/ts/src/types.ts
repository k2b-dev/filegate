export type Metadata = Record<string, unknown>;
export interface Ownership { uid?: number; gid?: number; mode?: string; dirMode?: string }
export interface WriteOptions { onConflict?: "error" | "overwrite" | "rename"; ownership?: Ownership; metadata?: Metadata }
export interface Node { root: string; path: string; id?: string; directory: boolean; size: number; modified: string; mode: string; uid: number; gid: number }
export interface Page { items: Node[]; next?: string }
export interface Stats { files: number; directories: number; bytes: number; updated: string; source: "filesystem" | "index" }
export interface Keep { last: number; hourly: number; daily: number; weekly: number; monthly: number }
export interface IndexStatus { enabled: boolean; rebuilding: boolean; scanned: number; lastBuilt: string | null; durationMs: number; error?: string }
export interface RootInfo { name: string; index: IndexStatus; stats: Stats | null; versioning: { enabled: boolean; keep: Keep }; cooldown: string; versions: number; versionBytes: number; filesystem: string; capacity: number; available: number; activeUploads: number; stagingBytes: number }
export interface System { version: string; started: string; uptimeSeconds: number; ready: boolean; maintenanceError?: string }
export interface Version { id: string; fileId: string; created: string; size: number; pinned: boolean; metadata?: Metadata; copyMode: "copy" | "reflink" }
export interface VersionOptions { pinned?: boolean; metadata?: Metadata }
export interface DirectURL { url: string; method: "PUT" | "GET"; expires: string }
export interface Session { id: string; root: string; path: string; size: number; chunkSize: number; expires: string; options: WriteOptions; segments: Record<string, string>; received: number; result?: Node }
export interface SessionCreated extends Session { url: string }
export interface ErrorResponse { error: string; message: string }
