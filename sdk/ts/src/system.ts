import { ClientCore } from "./core.js";
import type {
  HealthResponse,
  PruneResponse,
  SystemInfoResponse,
  SystemRuntimeResponse,
  UploadSessionListResponse,
  UploadSessionPhase,
} from "./types.js";

/**
 * Operational endpoints.
 *
 * `info` probes the mounts, so read it occasionally. `runtime` is made of
 * in-memory counters and is the one to poll for a live dashboard.
 */
export class SystemClient {
  constructor(private readonly core: ClientCore) {}

  /** Build, mounts and effective limits. Touches the filesystem; not for polling. */
  async info(): Promise<SystemInfoResponse> {
    return this.core.doJSON<SystemInfoResponse>("GET", "/v1/system/info");
  }

  /** Live counters: detector, worker pool, caches, upload sessions. Safe to poll. */
  async runtime(): Promise<SystemRuntimeResponse> {
    return this.core.doJSON<SystemRuntimeResponse>("GET", "/v1/system/runtime");
  }

  /**
   * Dependency health. Unlike the bare /health liveness probe this checks the
   * index, detector and mounts, and the server answers 503 when a check fails,
   * so callers that treat non-2xx as an error still get the body.
   */
  async health(): Promise<HealthResponse> {
    return this.core.doJSON<HealthResponse>("GET", "/v1/health");
  }

  /**
   * Runs a version-retention round on demand.
   *
   * Deletes data, so it is recorded in the activity log. Answers 409 while a
   * round is already in flight rather than starting a second scan.
   */
  async prune(): Promise<PruneResponse> {
    return this.core.doJSON<PruneResponse>("POST", "/v1/versions/prune");
  }

  /** Resumable upload sessions, optionally filtered by phase. */
  async uploadSessions(options: { phase?: UploadSessionPhase } = {}): Promise<UploadSessionListResponse> {
    const query = options.phase ? { phase: options.phase } : undefined;
    return this.core.doJSON<UploadSessionListResponse>("GET", "/v1/uploads/sessions", query);
  }
}
