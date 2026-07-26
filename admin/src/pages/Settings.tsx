import type { ConfigKeySchema, ConfigValue, ConfigValuesResponse, S3Key } from "@valentinkolb/filegate";
import { text } from "@valentinkolb/stdlib";
import { Layout } from "../components/Layout";

type SettingsData = {
  schema: ConfigKeySchema[];
  values: ConfigValuesResponse;
  keys: S3Key[];
  s3Enabled: boolean;
};

/** Groups keys by their leading path segment, which is already the section. */
function sections(schema: ConfigKeySchema[]): [string, ConfigKeySchema[]][] {
  const grouped = new Map<string, ConfigKeySchema[]>();
  for (const key of schema) {
    const section = key.path.split(".")[0] ?? "other";
    grouped.set(section, [...(grouped.get(section) ?? []), key]);
  }
  return [...grouped.entries()].sort(([a], [b]) => a.localeCompare(b));
}

/**
 * Human-readable rendering of a config value.
 *
 * Byte counts and durations are formatted rather than printed raw: 64 KiB reads
 * at a glance where 65536 does not. The exact value is still one click away in
 * the edit dialog, which is where a precise number actually matters.
 */
function displayValue(value: unknown, key?: ConfigKeySchema): string {
  if (value === null || value === undefined) return "-";

  // Secrets arrive as a presence flag, never as their content.
  if (typeof value === "object" && !Array.isArray(value) && "configured" in (value as Record<string, unknown>)) {
    return (value as { configured: boolean }).configured ? "configured" : "not set";
  }
  if (typeof value === "boolean") return value ? "true" : "false";

  if (key?.type === "retentionBuckets") return describeRetention(value);
  if (Array.isArray(value)) return value.length ? value.map((entry) => String(entry)).join(", ") : "-";

  if (key?.unit === "bytes" && typeof value === "number") {
    return value > 0 ? text.pprintBytes(value) : "0";
  }
  if (key?.type === "duration") return formatDurationValue(value);
  if (typeof value === "number") return text.pprintNumber(value);
  return String(value);
}

/** Raw value for the edit dialog, where precision is the point. */
function rawValue(value: unknown): string {
  if (value === null || value === undefined) return "";
  if (typeof value === "boolean") return value ? "true" : "false";
  if (Array.isArray(value)) return value.map((entry) => String(entry)).join(", ");
  if (typeof value === "object") return "";
  return String(value);
}

/**
 * Durations arrive as Go duration strings such as "15m0s". Reformatting through
 * stdlib drops the noise, and an unparseable value is shown as-is rather than
 * replaced by a guess.
 */
function formatDurationValue(value: unknown): string {
  const raw = String(value);
  const match = /^(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)s)?$/.exec(raw);
  if (!match) return raw;
  const hours = Number(match[1] ?? 0);
  const minutes = Number(match[2] ?? 0);
  const seconds = Number(match[3] ?? 0);
  const total = (hours * 3600 + minutes * 60 + seconds) * 1000;
  return total > 0 ? text.pprintDurationMs(total) : raw;
}

type RetentionBucket = { keep_for?: string; max_count?: number };

/**
 * Renders retention rules as sentences.
 *
 * These are objects, so the generic array path printed "[object Object]". The
 * rules are also the answer to "what happens to my versions?", which deserves
 * more than a raw dump.
 */
function describeRetention(value: unknown): string {
  if (!Array.isArray(value) || value.length === 0) return "no retention (versions accumulate)";
  return value
    .map((entry) => {
      const bucket = entry as RetentionBucket;
      const window = bucket.keep_for ? formatDurationValue(bucket.keep_for) : "?";
      const count = bucket.max_count;
      if (count === undefined || count < 0) return `all within ${window}`;
      return `max ${count} within ${window}`;
    })
    .join(" · ");
}

/** Only a runtime key that is neither secret nor structured is editable inline. */
function editable(key: ConfigKeySchema): boolean {
  return key.scope === "runtime" && !key.secret && !["s3Keys", "retentionBuckets"].includes(key.type);
}

export function Settings(props: SettingsData & { health?: "ok" | "degraded" | "fail"; mounts: number; error?: string; notice?: string }) {
  const byPath = new Map<string, ConfigValue>(props.values.values.map((value) => [value.path, value]));
  const restarts = props.values.restartRequired ?? [];

  return (
    <Layout
      active="settings"
      title="Settings"
      description="Every configuration key, where its value comes from, and which ones apply without a restart."
      mounts={props.mounts}
      health={props.health}
      error={props.error}
      notice={props.notice}
    >
      <div class="page-stack">
      {restarts.length > 0 && (
        <div class="panel restart-banner">
          <div class="panel-head">
            <h2>Restart required</h2>
          </div>
          <div class="panel-body">
            <p class="muted">These are stored but cannot take effect until Filegate restarts.</p>
            <ul class="restart-list">
              {restarts.map((entry) => (
                <li>
                  <code>{entry.path}</code>
                  <span class="muted">
                    running {entry.running || "(empty)"} → desired {entry.desired || "(empty)"}
                  </span>
                </li>
              ))}
            </ul>
          </div>
        </div>
      )}

      <div class="panel">
        <div class="panel-head">
          <h2>S3 access keys</h2>
          {props.s3Enabled && (
            <button class="btn" type="button" data-s3key-create>
              Create key
            </button>
          )}
        </div>
        <div class="panel-body">
          {!props.s3Enabled ? (
            <p class="muted">
              The S3 listener is disabled. Set <code>s3.enabled</code> and restart to manage access keys.
            </p>
          ) : props.keys.length === 0 ? (
            <p class="muted">No access keys. Creating one shows its secret exactly once.</p>
          ) : (
            <div class="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Access key</th>
                    <th>Buckets</th>
                    <th>Rate limit</th>
                    <th>State</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {props.keys.map((key) => (
                    <tr>
                      <td>
                        <code>{key.accessKey}</code>
                      </td>
                      <td>{key.buckets.join(", ")}</td>
                      <td>{key.requestsPerSecond ? `${key.requestsPerSecond}/s` : "-"}</td>
                      <td>{key.disabled ? <span class="tag warn">Disabled</span> : <span class="tag pin">Active</span>}</td>
                      <td class="row-actions">
                        <span class="actions">
                        <form method="post" action="/settings/s3keys/toggle">
                          <input type="hidden" name="accessKey" value={key.accessKey} />
                          <input type="hidden" name="disabled" value={key.disabled ? "false" : "true"} />
                          <button class="btn" type="submit">
                            {key.disabled ? "Enable" : "Disable"}
                          </button>
                        </form>
                        <button class="btn" type="button" data-s3key-rotate={key.accessKey}>
                          Rotate
                        </button>
                        <form method="post" action="/settings/s3keys/delete" data-confirm-s3key={key.accessKey}>
                          <input type="hidden" name="accessKey" value={key.accessKey} />
                          <button class="btn danger" type="submit">
                            Delete
                          </button>
                        </form>
                        </span>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>

      {sections(props.schema).map(([section, keys]) => (
        <div class="panel">
          <div class="panel-head">
            <h2>{section}</h2>
          </div>
          <div class="panel-body">
            <div class="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Key</th>
                    <th>Value</th>
                    <th>Source</th>
                    <th>Scope</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {keys.map((key) => {
                    const current = byPath.get(key.path);
                    return (
                      <tr>
                        <td>
                          <code>{key.path}</code>
                          <div class="muted setting-usage">{key.usage}</div>
                        </td>
                        <td>{displayValue(current?.value, key)}</td>
                        <td>
                          <span class={`tag source-${current?.source ?? "default"}`}>{current?.source ?? "default"}</span>
                        </td>
                        <td>
                          {key.scope === "static" ? (
                            <span class="tag warn" title={key.reason}>
                              static
                            </span>
                          ) : (
                            <span class="tag pin">runtime</span>
                          )}
                        </td>
                        <td class="row-actions">
                          <span class="actions">
                          {editable(key) && (
                            <button
                              class="btn"
                              type="button"
                              data-setting-edit={key.path}
                              data-setting-type={key.type}
                              data-setting-value={rawValue(current?.value)}
                            >
                              Edit
                            </button>
                          )}
                          {current?.source === "runtime" && (
                            <form method="post" action="/settings/reset">
                              <input type="hidden" name="path" value={key.path} />
                              <button class="btn" type="submit" title="Drop the runtime override and fall back to the file or default">
                                Reset
                              </button>
                            </form>
                          )}
                          </span>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </div>
        </div>
      ))}
      </div>
      <script src="/settings.js" defer />
    </Layout>
  );
}
