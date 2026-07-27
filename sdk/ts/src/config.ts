import { ClientCore } from "./core.js";
import type {
  ConfigManifestApplyResponse,
  ConfigManifestPlanResponse,
  ConfigSchemaResponse,
  ConfigValuesResponse,
  S3Key,
  S3KeyCreateRequest,
  S3KeyCreated,
  S3KeyListResponse,
  S3KeyUpdateRequest,
} from "./types.js";

/**
 * Configuration and runtime resources.
 *
 * Schema is what lets a client render controls without hardcoding the key list:
 * a key added in a later release shows up on its own.
 */
export class ConfigClient {
  constructor(private readonly core: ClientCore) {}

  /** Every key with its type, scope, default and usage. */
  async schema(): Promise<ConfigSchemaResponse> {
    return this.core.doJSON<ConfigSchemaResponse>("GET", "/v1/config/schema");
  }

  /** Desired and effective values, provenance, and manifest metadata. */
  async values(): Promise<ConfigValuesResponse> {
    return this.core.doJSON<ConfigValuesResponse>("GET", "/v1/config");
  }

  /** Plan a complete replacement manifest without changing server state. */
  async plan(values: Record<string, unknown>): Promise<ConfigManifestPlanResponse> {
    return this.core.doJSON<ConfigManifestPlanResponse>(
      "POST",
      "/v1/config/plan",
      undefined,
      JSON.stringify({ values }),
      "application/json",
    );
  }

  /** Apply a complete manifest if the revision returned by plan is still current. */
  async apply(values: Record<string, unknown>, expectedRevision: string): Promise<ConfigManifestApplyResponse> {
    return this.core.doJSON<ConfigManifestApplyResponse>(
      "POST",
      "/v1/config/apply",
      undefined,
      JSON.stringify({ values, expectedRevision }),
      "application/json",
    );
  }
}

/** S3 access keys. Secrets are returned only by create and rotate. */
export class S3KeysClient {
  constructor(private readonly core: ClientCore) {}

  async list(): Promise<S3KeyListResponse> {
    return this.core.doJSON<S3KeyListResponse>("GET", "/v1/s3/keys");
  }

  /** The returned secret is the only copy; it cannot be read again. */
  async create(req: S3KeyCreateRequest): Promise<S3KeyCreated> {
    return this.core.doJSON<S3KeyCreated>("POST", "/v1/s3/keys", undefined, JSON.stringify(req), "application/json");
  }

  async update(accessKey: string, req: S3KeyUpdateRequest): Promise<S3Key> {
    return this.core.doJSON<S3Key>("PATCH", `/v1/s3/keys/${encodeURIComponent(accessKey)}`, undefined, JSON.stringify(req), "application/json");
  }

  /** Issues a new secret, keeping the key's grants. */
  async rotate(accessKey: string): Promise<S3KeyCreated> {
    return this.core.doJSON<S3KeyCreated>("POST", `/v1/s3/keys/${encodeURIComponent(accessKey)}/rotate`);
  }

  async delete(accessKey: string): Promise<void> {
    await this.core.doRaw("DELETE", `/v1/s3/keys/${encodeURIComponent(accessKey)}`);
  }
}
