export { Filegate, filegate, type FilegateConfig } from "./client.js";
export { ActivityClient, type ActivityListConfig } from "./activity.js";
export { CapabilitiesClient } from "./capabilities.js";
export { DownloadsClient } from "./downloads.js";
export { FilegateError } from "./errors.js";
export type { FetchImpl } from "./core.js";

export type { GetPathOptions, PathPutResponse, PutPathOptions } from "./paths.js";
export type { ThumbnailOptions } from "./nodes.js";
export {
  directUploads,
  upload,
  uploadDirect,
  type BrowserUploadAllowItem,
  type BrowserUploadAllowRequest,
  type BrowserUploadAllowResponse,
  type BrowserUploadAllowResult,
  type BrowserUploadConfig,
  type BrowserUploadConflictMode,
  type BrowserUploadEvent,
  type BrowserUploadFile,
  type BrowserUploadRequest,
  type BrowserUploadResult,
  type BrowserUploadSegment,
  type DirectUploadFinish,
  type DirectUploadOptions,
  type DirectUploadResult,
  type DirectUploadSegmentRequest,
  type DirectUploadSessionRequest,
  type UploadSessionAbortRequest,
  type UploadSessionCommitRequest,
  type UploadSessionPutSegmentRequest,
  type UploadSessionStatusRequest,
} from "./uploads.js";
export type { GlobOptions } from "./search.js";
export type {
  ListVersionsOptions,
  ListVersionsResponse,
  RestoreOptions,
  VersionPinRequest,
  VersionResponse,
  VersionRestoreRequest,
  VersionRestoreResponse,
  VersionSnapshotRequest,
} from "./versions.js";

export type { FileConflictMode, FingerprintMode, MkdirConflictMode, UploadSessionConflictMode } from "./types.js";

export type {
  ConfigKeySchema,
  ConfigManagedBy,
  ConfigManifestApplyResponse,
  ConfigManifestChange,
  ConfigManifestPlanResponse,
  ConfigManifestStatus,
  RetentionBucket,
  ConfigRestartRequired,
  ConfigSchemaResponse,
  ConfigScope,
  ConfigSource,
  ConfigValue,
  ConfigValuesResponse,
  S3Key,
  S3KeyCreated,
  S3KeyCreateRequest,
  S3KeyListResponse,
  S3KeyUpdateRequest,
} from "./types.js";

export type {
  BuildInfo,
  CacheRuntime,
  DetectorInfo,
  DetectorRuntime,
  HealthCheck,
  HealthResponse,
  HealthStatus,
  JobsRuntime,
  LifecycleRuntime,
  PruneResponse,
  LimitsInfo,
  MountInfo,
  SystemInfoResponse,
  SystemRuntimeResponse,
  UploadSessionListResponse,
  UploadSessionPhase,
  UploadSessionSummary,
  UploadSessionsRuntime,
  VersioningInfo,
} from "./types.js";

// Pure helpers are intentionally NOT re-exported here. Import them from the
// dedicated entrypoint to keep tree-shaking honest:
//   import { uploads } from "@k2b/filegate/utils";

export type {
  ActivityActor,
  ActivityEvent,
  ActivityListResponse,
  ActivityTarget,
  DirectUploadURLRequest,
  DirectUploadURLResponse,
  DirectDownloadURLRequest,
  DirectDownloadURLResponse,
  CapabilitiesResponse,
  ErrorResponse,
  GlobSearchError,
  GlobSearchMeta,
  GlobSearchPath,
  GlobSearchResponse,
  IndexResolveManyResponse,
  IndexResolveRequest,
  IndexResolveSingleResponse,
  MkdirRequest,
  Node,
  NodeListResponse,
  OKResponse,
  Ownership,
  OwnershipView,
  StatsCache,
  StatsDisk,
  StatsIndex,
  StatsMount,
  StatsResponse,
  StatsSystem,
  TransferRequest,
  TransferResponse,
  UploadSegmentResponse,
  UploadCapabilities,
  UploadSessionBatchCreateRequest,
  UploadSessionBatchCreateResponse,
  UploadSessionCommitResponse,
  UploadSessionCreateRequest,
  UploadSessionDirect,
  UploadSessionDirectRequest,
  UploadSessionResponse,
  UploadSessionSegment,
  UpdateNodeRequest,
} from "./types.js";
