import { ClientCore, type FetchImpl } from "./core.js";
import { ActivityClient } from "./activity.js";
import { CapabilitiesClient } from "./capabilities.js";
import { DownloadsClient } from "./downloads.js";
import { IndexClient } from "./index-client.js";
import { NodesClient } from "./nodes.js";
import { PathsClient } from "./paths.js";
import { SearchClient } from "./search.js";
import { StatsClient } from "./stats.js";
import { ConfigClient, S3KeysClient } from "./config.js";
import { SystemClient } from "./system.js";
import { TransfersClient } from "./transfers.js";
import { UploadsClient } from "./uploads.js";
import { VersionsClient } from "./versions.js";

export interface FilegateConfig {
  baseUrl: string;
  token: string;
  fetchImpl?: FetchImpl;
  userAgent?: string;
  defaultHeaders?: Record<string, string>;
}

// Note: pure helpers (chunk math, hashing) live at "@valentinkolb/filegate/utils".
// They intentionally do not hang off this class so callers can use them
// without constructing a Filegate (e.g. in browsers without a token).
export class Filegate {
  readonly paths: PathsClient;
  readonly nodes: NodesClient;
  readonly uploads: UploadsClient;
  readonly transfers: TransfersClient;
  readonly search: SearchClient;
  readonly index: IndexClient;
  readonly stats: StatsClient;
  readonly system: SystemClient;
  readonly config: ConfigClient;
  readonly s3Keys: S3KeysClient;
  readonly capabilities: CapabilitiesClient;
  readonly versions: VersionsClient;
  readonly downloads: DownloadsClient;
  readonly activity: ActivityClient;

  readonly baseUrl: string;

  constructor(cfg: FilegateConfig) {
    if (!cfg.baseUrl?.trim()) throw new Error("baseUrl is required");
    if (!cfg.token?.trim()) throw new Error("token is required");

    const core = new ClientCore({
      baseURL: cfg.baseUrl,
      token: cfg.token,
      fetchImpl: cfg.fetchImpl ?? globalThis.fetch.bind(globalThis),
      userAgent: cfg.userAgent,
      defaultHeaders: cfg.defaultHeaders,
    });

    this.baseUrl = core.baseURL;
    this.paths = new PathsClient(core);
    this.nodes = new NodesClient(core);
    this.uploads = new UploadsClient(core);
    this.transfers = new TransfersClient(core);
    this.search = new SearchClient(core);
    this.index = new IndexClient(core);
    this.stats = new StatsClient(core);
    this.system = new SystemClient(core);
    this.config = new ConfigClient(core);
    this.s3Keys = new S3KeysClient(core);
    this.capabilities = new CapabilitiesClient(core);
    this.versions = new VersionsClient(core);
    this.downloads = new DownloadsClient(core);
    this.activity = new ActivityClient(core);
  }
}

/**
 * Lazy default instance, created on first property access.
 * Reads FILEGATE_URL and FILEGATE_TOKEN from environment (Node/Bun only).
 */
function createDefaultInstance(): Filegate {
  const baseUrl = (globalThis as any).process?.env?.FILEGATE_URL;
  const token = (globalThis as any).process?.env?.FILEGATE_TOKEN;
  if (!baseUrl) throw new Error("FILEGATE_URL environment variable is not set");
  if (!token) throw new Error("FILEGATE_TOKEN environment variable is not set");
  return new Filegate({ baseUrl, token });
}

let _default: Filegate | undefined;

/** Default env-based instance (server runtimes only). Created lazily on first use. */
export const filegate: Filegate = new Proxy({} as Filegate, {
  get(_target, prop, receiver) {
    if (!_default) _default = createDefaultInstance();
    return Reflect.get(_default, prop, receiver);
  },
});
