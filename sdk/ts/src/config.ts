import { ClientCore } from "./core.js";
import type {
  ConfigChangeResponse,
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

  /** Effective values, where each came from, and anything awaiting a restart. */
  async values(): Promise<ConfigValuesResponse> {
    return this.core.doJSON<ConfigValuesResponse>("GET", "/v1/config");
  }

  /**
   * Apply a batch of changes. A null value clears an override so the key falls
   * back to the file or its default.
   *
   * A static key is stored but cannot take effect until a restart; it comes
   * back in restartRequired rather than being silently ignored.
   */
  async patch(changes: Record<string, unknown>): Promise<ConfigChangeResponse> {
    return this.core.doJSON<ConfigChangeResponse>("PATCH", "/v1/config", undefined, JSON.stringify({ changes }), "application/json");
  }

  /** Validate without applying, for checking input as it is typed. */
  async validate(changes: Record<string, unknown>): Promise<ConfigChangeResponse> {
    return this.core.doJSON<ConfigChangeResponse>("POST", "/v1/config/validate", undefined, JSON.stringify({ changes }), "application/json");
  }

  /** Re-read every source, for a config file edited by hand. */
  async reload(): Promise<ConfigChangeResponse> {
    return this.core.doJSON<ConfigChangeResponse>("POST", "/v1/config/reload");
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
