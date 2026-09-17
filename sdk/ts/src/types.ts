export type Metadata = Record<string, unknown>;
export interface Ownership { uid?: number; gid?: number; mode?: string; dirMode?: string }
export type ACLScope = "access" | "default";
export type ACLPermissions = "---" | "--x" | "-w-" | "-wx" | "r--" | "r-x" | "rw-" | "rwx";
export type ACLEntry =
  | { tag: "owner" | "owningGroup" | "mask" | "other"; id?: never; permissions: ACLPermissions }
  | { tag: "user" | "group"; id: number; permissions: ACLPermissions };
export type ACLTag = ACLEntry["tag"];
export interface ACL { entries: ACLEntry[] }
export interface DirectoryACLs { access?: ACL; default?: ACL }
export interface DirectoryOptions { ownership?: Ownership; acl?: DirectoryACLs }
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
/** Omitted dimensions default to 256; dimensions must be integers from 1 to 2048. */
export interface ThumbnailLeaseOptions { width?: number; height?: number; expiresIn?: number }
export interface DirectURL { url: string; method: "PUT" | "GET"; expires: string }
export type SessionState = "open" | "committed" | "aborted" | "expired";
/** Minimal transfer status exposed by a session lease. */
export interface SessionStatus { id: string; root: string; size: number; chunkSize: number; expires: string; state: SessionState; segments: Record<string, string>; received: number; terminalAt?: string; retainUntil?: string }
export interface Session extends SessionStatus { path: string; options: WriteOptions; result?: Node }
export interface SessionLeaseRequest { expiresIn?: number; allowAbort?: boolean }
export interface SessionLease { url: string; expires: string; operations: ("status" | "write" | "abort")[] }
export interface SessionCreated { session: Session; lease: SessionLease }
export interface ArchiveItem { root: string; path: string; archivePath: string }
export interface ArchiveLease { url: string; method: "POST"; expires: string; manifest: string }
export interface ErrorResponse { error: string; message: string }
