/** Ownership input for create/update operations. */
export interface Ownership {
  uid?: number;
  gid?: number;
  mode?: string;
  dirMode?: string;
}

/** Ownership as returned by the server. */
export interface OwnershipView {
  uid: number;
  gid: number;
  mode: string;
}

/** A file or directory node.
 *
 * `nextCursor` is an opaque pagination token: pass it back verbatim as
 * the `cursor` option to fetch the next page of children. Do not
 * construct or interpret it. */
export interface Node {
  id: string;
  type: "file" | "directory";
  name: string;
  path: string;
  size: number;
  mtime: number;
  ownership: OwnershipView;
  mimeType?: string;
  etag?: string;
  sha256?: string;
  exif: Record<string, string>;
  children?: Node[];
  pageSize?: number;
  nextCursor?: string;
}

/** Response for root listing (GET /v1/paths/). */
export interface NodeListResponse {
  items: Node[];
  total: number;
}

/** OK acknowledgement. */
export interface OKResponse {
  ok: boolean;
}

/** Error response body. On a 409 Conflict, `existingId` and `existingPath`
 * are populated so the client can render a "what should we do?" prompt
 * without an extra resolve call. Both undefined otherwise. */
export interface ErrorResponse {
  error: string;
  existingId?: string;
  existingPath?: string;
}

/** Conflict mode accepted by file-write endpoints. `"skip"` is mkdir-only and
 * not allowed here. Upload sessions narrow this to `"error" | "overwrite"`. */
export type FileConflictMode = "error" | "overwrite" | "rename";
export type UploadSessionConflictMode = Exclude<FileConflictMode, "rename">;
export type FingerprintMode = "none" | "cached" | "ensure";

/** Conflict mode accepted by mkdir. `"overwrite"` is forbidden — replacing
 * a directory subtree is a Transfer operation, not a mkdir one. */
export type MkdirConflictMode = "error" | "skip" | "rename";

// --- Stats ---

export interface StatsIndex {
  totalEntities: number;
  totalFiles: number;
  totalDirs: number;
  dbSizeBytes: number;
}

export interface StatsCache {
  pathEntries: number;
  pathCapacity: number;
  pathUtilRatio: number;
}

export interface StatsMount {
  id: string;
  name: string;
  path: string;
  files: number;
  dirs: number;
}

export interface StatsDisk {
  diskName: string;
  fsType: string;
  used: number;
  size: number;
  roots: string[];
}

export interface StatsSystem {
  goroutines: number;
  heapAllocBytes: number;
  heapSysBytes: number;
  heapObjects: number;
  numGC: number;
  lastGCPauseNs: number;
  openFDs: number;
  maxFDs: number;
}

export interface StatsResponse {
  generatedAt: number;
  index: StatsIndex;
  cache: StatsCache;
  mounts: StatsMount[];
  disks: StatsDisk[];
  system: StatsSystem;
}

// --- Activity ---

export interface ActivityActor {
  kind: "system" | "bearer_token" | "s3_key" | "signed_url" | string;
  id: string;
  label?: string;
  delegatedActor?: string;
}

export interface ActivityTarget {
  kind: string;
  id?: string;
  path?: string;
}

export interface ActivityEvent {
  id: string;
  at: number;
  actor: ActivityActor;
  operation: string;
  outcome: "succeeded" | "failed" | "skipped" | string;
  target?: ActivityTarget;
  durationMs?: number;
  requestId?: string;
  error?: string;
  meta?: Record<string, string | number | boolean>;
}

export interface ActivityListResponse {
  items: ActivityEvent[];
  total: number;
  offset: number;
  limit: number;
  retained: number;
  capacity: number;
  operations: string[];
}

// --- Capabilities ---

export interface CapabilitiesResponse {
  uploads: UploadCapabilities;
}

export interface UploadCapabilities {
  maxChunkBytes: number;
  maxUploadBytes: number;
  maxSessionUploadBytes: number;
  maxConcurrentSegmentWrites: number;
}

// --- Mkdir ---

export interface MkdirRequest {
  path: string;
  recursive?: boolean;
  ownership?: Ownership;
  /** Default `"error"`. `"skip"` returns the existing directory unchanged
   * if one with the same name exists. `"rename"` picks a unique sibling
   * name and creates a fresh directory there. `"overwrite"` is rejected. */
  onConflict?: MkdirConflictMode;
}

// --- Update ---

export interface UpdateNodeRequest {
  name?: string;
  ownership?: Ownership;
}

// --- Transfer ---

export interface TransferRequest {
  op: "move" | "copy";
  sourceId: string;
  targetParentId: string;
  targetName: string;
  /** Default `"error"`. Same vocabulary as the other file-write endpoints —
   * see `FileConflictMode`. */
  onConflict?: FileConflictMode;
  ownership?: Ownership;
}

export interface TransferResponse {
  node: Node;
  op: string;
}

// --- Search ---

export interface GlobSearchError {
  path: string;
  cause: string;
}

export interface GlobSearchPath {
  path: string;
  returned: number;
  hasMore: boolean;
}

export interface GlobSearchMeta {
  pattern: string;
  limit: number;
  resultCount: number;
  errorCount: number;
}

export interface GlobSearchResponse {
  results: Node[];
  errors: GlobSearchError[];
  meta: GlobSearchMeta;
  paths: GlobSearchPath[];
}

// --- Direct uploads ---

export interface DirectUploadURLRequest {
	path: string;
	expiresInSeconds?: number;
	contentType?: string;
	onConflict?: FileConflictMode;
	maxBytes?: number;
}

export interface DirectUploadURLResponse {
  uploadUrl: string;
  method: "PUT";
  path: string;
  expiresAt: number;
  maxBytes: number;
}

// --- Direct downloads ---

export interface DirectDownloadURLRequest {
  nodeId?: string;
  path?: string;
  expiresInSeconds?: number;
  inline?: boolean;
}

export interface DirectDownloadURLResponse {
  downloadUrl: string;
  method: "GET";
  expiresAt: number;
  node: Node;
}

// --- Upload sessions ---

export interface UploadSessionDirectRequest {
  expiresInSeconds?: number;
  allow?: Array<"putSegment" | "status" | "commit" | "abort">;
}

export interface UploadSessionCreateRequest {
	path: string;
	size: number;
	checksum: string;
	segmentSize?: number;
	contentType?: string;
	ownership?: Ownership;
	onConflict?: UploadSessionConflictMode;
	direct?: UploadSessionDirectRequest;
}

export interface UploadSessionBatchCreateRequest {
  uploads: UploadSessionCreateRequest[];
  segmentSize?: number;
  direct?: UploadSessionDirectRequest;
}

export interface UploadSessionSegment {
  index: number;
  offset: number;
  size: number;
}

export interface UploadSessionDirect {
  baseUrl: string;
  token: string;
  expiresAt: number;
  allow: Array<"putSegment" | "status" | "commit" | "abort">;
}

export interface UploadSessionResponse {
  id: string;
  path: string;
  size: number;
  checksum: string;
  segmentSize: number;
  totalSegments: number;
  segments: UploadSessionSegment[];
  uploadedSegments: number[];
  phase: "in_progress" | "committing" | "committed" | "aborted";
  direct?: UploadSessionDirect;
}

export interface UploadSessionBatchCreateResponse {
  sessions: UploadSessionResponse[];
}

export interface UploadSegmentResponse {
  sessionId: string;
  index: number;
  uploadedSegments: number[];
}

export interface UploadSessionCommitResponse {
  node: Node;
  checksum: string;
}

// --- Index resolve ---

export interface IndexResolveRequest {
  path?: string;
  paths?: string[];
  id?: string;
  ids?: string[];
}

export interface IndexResolveSingleResponse {
  item: Node | null;
}

export interface IndexResolveManyResponse {
  items: (Node | null)[];
  total: number;
}

/** Phase of a resumable upload session. */
export type UploadSessionPhase = "in_progress" | "committing" | "committed" | "aborted";

export interface BuildInfo {
  version: string;
  commit: string;
  go: string;
}

/**
 * A configured mount and the result of its health probe. `writable` and
 * `xattrSupported` are the two properties whose absence breaks Filegate
 * silently: no writes, and no stable file IDs.
 */
export interface MountInfo {
  name: string;
  path: string;
  exists: boolean;
  writable: boolean;
  xattrSupported: boolean;
  freeBytes: number;
  totalBytes: number;
  errors?: string[];
}

export interface VersioningInfo {
  enabled: boolean;
  mode: string;
  cooldownMs: number;
  prunerIntervalMs: number;
  maxPinnedPerFile: number;
}

/** Curated, non-secret configuration needed to interpret server rejections. */
export interface LimitsInfo {
  maxChunkBytes: number;
  maxUploadBytes: number;
  maxSessionUploadBytes: number;
  maxConcurrentSegmentWrites: number;
  uploadMinFreeBytes: number;
  uploadExpiryMs: number;
  uploadCleanupIntervalMs: number;
  thumbnailMaxSourceBytes: number;
  thumbnailMaxPixels: number;
  pathCacheCapacity: number;
  activityRingCapacity: number;
}

export interface DetectorInfo {
  backend: string;
  intervalMs: number;
}

export interface SystemInfoResponse {
  generatedAt: number;
  build: BuildInfo;
  startedAt: number;
  uptimeMs: number;
  detector: DetectorInfo;
  versioning: VersioningInfo;
  limits: LimitsInfo;
  mounts: MountInfo[];
  indexPath: string;
}

/**
 * Live detector state. `staleForMs` growing far past `intervalMs` means
 * detection stopped, which causes silent index drift rather than an outage.
 */
export interface DetectorRuntime {
  backend: string;
  intervalMs: number;
  cycles: number;
  lastScanAt: number;
  lastScanDurationMs: number;
  staleForMs: number;
  errors: number;
  pendingBatches: number;
  queueCapacity: number;
  trackedDirs?: number;
  trackedFiles?: number;
  generations?: Record<string, number>;
}

/** Worker pool pressure. `queued` nearing `queueCapacity` precedes 503s. */
export interface JobsRuntime {
  workers: number;
  queued: number;
  queueCapacity: number;
  inFlight: number;
  rejected: number;
  panics: number;
}

export interface CacheRuntime {
  entries: number;
  capacity: number;
  hits: number;
  misses: number;
  hitRatio: number;
}

export interface UploadSessionsRuntime {
  inProgress: number;
  committing: number;
  committed: number;
  aborted: number;
  writeSlotsInUse: number;
  writeSlotsLimit: number;
}

/** Last background maintenance run. lastPruneAt of 0 means none completed yet. */
export interface LifecycleRuntime {
  prunerIntervalMs: number;
  lastPruneAt: number;
  lastPruneDurationMs: number;
  nextPruneAt: number;
  pruneRuns: number;
  filesScanned: number;
  versionsKept: number;
  versionsDeleted: number;
  orphansPurged: number;
  blobsDeleted: number;
  pruneErrors: number;
  pruneError?: string;
}

export interface SystemRuntimeResponse {
  generatedAt: number;
  detector: DetectorRuntime;
  jobs: JobsRuntime;
  pathCache: CacheRuntime;
  thumbnailCache: CacheRuntime;
  uploadSessions: UploadSessionsRuntime;
  lifecycle: LifecycleRuntime;
}

export type HealthStatus = "ok" | "degraded" | "fail";

export interface HealthCheck {
  name: string;
  status: HealthStatus;
  detail?: string;
}

export interface HealthResponse {
  status: HealthStatus;
  generatedAt: number;
  checks: HealthCheck[];
}

export interface UploadSessionSummary {
  id: string;
  path: string;
  size: number;
  segmentSize: number;
  totalSegments: number;
  uploadedSegments: number;
  uploadedBytes: number;
  phase: UploadSessionPhase;
  createdAt: number;
  updatedAt: number;
  ageMs: number;
  contentType?: string;
}

export interface UploadSessionListResponse {
  items: UploadSessionSummary[];
  total: number;
}

export type ConfigScope = "static" | "runtime";
export type ConfigSource = "default" | "file" | "env" | "runtime";

export interface ConfigKeySchema {
  path: string;
  /** string, bool, int, duration, stringList, s3Keys or retentionBuckets. */
  type: string;
  scope: ConfigScope;
  usage: string;
  /** Why a static key cannot change while the server runs. */
  reason?: string;
  /** "bytes" for a byte count, absent otherwise. Lets a client render 65536 as 64 KiB. */
  unit?: string;
  /** Secret values report presence only, never their content. */
  secret: boolean;
  default?: unknown;
}

export interface ConfigSchemaResponse {
  keys: ConfigKeySchema[];
}

export interface ConfigValue {
  path: string;
  /** The effective value, or {configured: boolean} for secrets. */
  value: unknown;
  source: ConfigSource;
  scope: ConfigScope;
}

export interface ConfigRestartRequired {
  path: string;
  running: string;
  desired: string;
}

export interface ConfigValuesResponse {
  generatedAt: number;
  values: ConfigValue[];
  restartRequired?: ConfigRestartRequired[];
}

export interface ConfigChangeResponse {
  applied: boolean;
  restartRequired?: ConfigRestartRequired[];
}

export interface S3Key {
  accessKey: string;
  buckets: string[];
  requestsPerSecond?: number;
  burst?: number;
  disabled: boolean;
  createdAt: number;
  updatedAt: number;
}

/** Carries the one-time secret; it cannot be read again afterwards. */
export interface S3KeyCreated extends S3Key {
  secretKey: string;
}

export interface S3KeyListResponse {
  items: S3Key[];
  total: number;
}

export interface S3KeyCreateRequest {
  accessKey?: string;
  /** Required. Use ["*"] to grant every mount. */
  buckets: string[];
  requestsPerSecond?: number;
  burst?: number;
}

export interface S3KeyUpdateRequest {
  buckets?: string[];
  requestsPerSecond?: number;
  burst?: number;
  disabled?: boolean;
}
