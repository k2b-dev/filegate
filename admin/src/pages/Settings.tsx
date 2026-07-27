import type { ConfigKeySchema, ConfigValue, ConfigValuesResponse, S3Key } from "@valentinkolb/filegate";
import { text } from "@valentinkolb/stdlib";
import { Layout } from "../components/Layout";
import { Icon, IconLabel } from "../components/Icons";
import { tidyDuration } from "../retention";

type SettingsData = {
  schema: ConfigKeySchema[];
  values: ConfigValuesResponse;
  keys: S3Key[];
  s3Enabled: boolean;
  mountNames: string[];
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
  if (typeof value === "number") {
    // Naming a key "size" does not make it bytes: several hold entry counts,
    // so the unit is spelled out rather than guessed from the name.
    const count = text.pprintNumber(value);
    return key?.unit ? `${count} ${key.unit}` : count;
  }
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

type Bucket = { keepFor?: string; maxCount?: number };

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
      const bucket = entry as Bucket;
      const window = bucket.keepFor ? formatDurationValue(bucket.keepFor) : "?";
      const count = bucket.maxCount;
      if (count === undefined || count < 0) return `keep all within ${window}`;
      return `keep ${count} within ${window}`;
    })
    .join(" · ");
}

/**
 * Editable form of the retention policy, one rule per line.
 *
 * Reuses the syntax the CLI flag already documents (keep_for=1h,max_count=-1)
 * instead of inventing a second one for the UI.
 */
function retentionAsText(value: unknown): string {
  if (!Array.isArray(value)) return "";
  return value
    .map((entry) => {
      const bucket = entry as Bucket;
      return `keep_for=${tidyDuration(bucket.keepFor ?? "")},max_count=${bucket.maxCount ?? -1}`;
    })
    .join("; ");
}

/** Only a runtime key that is neither secret nor structured is editable inline. */
function editable(key: ConfigKeySchema): boolean {
  // s3Keys stays out because access keys are managed as resources above, with
  // rotation and an audit trail, not as a config value.
  return key.scope === "runtime" && !key.secret && key.type !== "s3Keys";
}

const acronyms: Record<string, string> = {
  api: "API",
  cors: "CORS",
  http2: "HTTP/2",
  lru: "LRU",
  s3: "S3",
  url: "URL",
  v1: "V1",
};

function words(segment: string): string {
  return segment
    .split("_")
    .map((word) => acronyms[word] ?? word)
    .join(" ");
}

function sentence(segment: string): string {
  const label = words(segment);
  return label.charAt(0).toUpperCase() + label.slice(1);
}

/** Gives nested settings enough context without repeating the full code path. */
function settingLabel(path: string): string {
  const parts = path.split(".").slice(1);
  const leaf = sentence(parts.at(-1) ?? path);
  if (parts.length < 2) return leaf;
  return `${sentence(parts.at(-2) ?? "")} · ${leaf}`;
}

function sectionLabel(section: string): string {
  return acronyms[section] ?? sentence(section);
}

function sourceLabel(source: ConfigValue["source"] | undefined): string {
  switch (source) {
    case "env":
      return "Environment";
    case "file":
      return "Config file";
    case "runtime":
      return "Live override";
    default:
      return "Default";
  }
}

function isConfigured(value: unknown): boolean | undefined {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return undefined;
  if (!("configured" in value)) return undefined;
  return Boolean((value as { configured?: unknown }).configured);
}

function SwitchControl(props: {
  checked: boolean;
  label: string;
  name?: string;
  value?: string;
}) {
  const state = props.checked ? "On" : "Off";
  return (
    <button
      class="setting-switch"
      type="submit"
      role="switch"
      aria-checked={props.checked ? "true" : "false"}
      aria-label={`${props.label}: ${state}. Change to ${props.checked ? "off" : "on"}`}
      name={props.name}
      value={props.value}
    >
      <span class="setting-switch-track" aria-hidden="true">
        <span class="setting-switch-knob" />
      </span>
      <span class="setting-switch-state">{state}</span>
    </button>
  );
}

function BoolControl(props: { key: ConfigKeySchema; current?: ConfigValue }) {
  const checked = Boolean(props.current?.value);
  const label = settingLabel(props.key.path);

  if (editable(props.key)) {
    return (
      <form method="post" action="/settings/apply">
        <input type="hidden" name="path" value={props.key.path} />
        <input type="hidden" name="type" value={props.key.type} />
        <SwitchControl checked={checked} label={label} name="value" value={checked ? "false" : "true"} />
      </form>
    );
  }
  return <strong class="setting-state-value">{checked ? "On" : "Off"}</strong>;
}

function ChoiceControl(props: { key: ConfigKeySchema; current?: ConfigValue }) {
  const value = String(props.current?.value ?? "");
  if (!editable(props.key)) {
    return <strong class="setting-state-value is-choice">{value}</strong>;
  }

  return (
    <form class="setting-choice-form" method="post" action="/settings/apply">
      <input type="hidden" name="path" value={props.key.path} />
      <input type="hidden" name="type" value={props.key.type} />
      <select class="select setting-choice" name="value" aria-label={settingLabel(props.key.path)} data-setting-choice>
        {props.key.choices?.map((choice) => (
          <option value={choice} selected={choice === value}>
            {choice}
          </option>
        ))}
      </select>
      <button class="btn setting-choice-submit" type="submit">
        Apply
      </button>
    </form>
  );
}

function RetentionPreview(props: { value: unknown }) {
  if (!Array.isArray(props.value) || props.value.length === 0) {
    return <span class="setting-empty-value">Versions accumulate</span>;
  }
  return (
    <span class="setting-retention-preview">
      {props.value.map((entry) => {
        const bucket = entry as Bucket;
        return (
          <span>
            <strong>{bucket.keepFor ? formatDurationValue(bucket.keepFor) : "?"}</strong>
            {bucket.maxCount === undefined || bucket.maxCount < 0 ? "all" : `up to ${bucket.maxCount}`}
          </span>
        );
      })}
    </span>
  );
}

function ValuePreview(props: { key: ConfigKeySchema; value: unknown }) {
  const configured = isConfigured(props.value);
  if (configured !== undefined) {
    return (
      <span class={`setting-secret-state ${configured ? "is-configured" : ""}`}>
        <Icon name={configured ? "lock-check" : "lock-open"} />
        {configured ? "Configured" : "Not configured"}
      </span>
    );
  }
  if (props.key.type === "retentionBuckets") return <RetentionPreview value={props.value} />;
  if (props.key.type === "stringList") {
    const entries = Array.isArray(props.value) ? props.value : [];
    if (entries.length === 0) return <span class="setting-empty-value">None</span>;
    return <span class="setting-list-value">{entries.map((entry) => String(entry)).join(", ")}</span>;
  }
  return <strong class="setting-value-text">{displayValue(props.value, props.key)}</strong>;
}

function SettingActions(props: { key: ConfigKeySchema; current?: ConfigValue }) {
  const direct = props.key.type === "bool" || Boolean(props.key.choices?.length);
  return (
    <span class="setting-actions">
      {editable(props.key) && !direct && (
        <button
          class="btn"
          type="button"
          data-setting-edit={props.key.path}
          data-setting-type={props.key.type}
          data-setting-unit={props.key.unit}
          data-setting-usage={props.key.usage}
          data-setting-value={
            props.key.type === "retentionBuckets" ? retentionAsText(props.current?.value) : rawValue(props.current?.value)
          }
          data-setting-json={
            props.key.type === "retentionBuckets" ? JSON.stringify(props.current?.value ?? []) : undefined
          }
        >
          <IconLabel icon="pencil">Edit</IconLabel>
        </button>
      )}
      {props.current?.source === "runtime" && (
        <form method="post" action="/settings/reset">
          <input type="hidden" name="path" value={props.key.path} />
          <button class="btn" type="submit" title="Drop the live override and fall back to the config file or default">
            <IconLabel icon="restore">Reset</IconLabel>
          </button>
        </form>
      )}
    </span>
  );
}

function SettingRow(props: { key: ConfigKeySchema; current?: ConfigValue }) {
  const isChoice = Boolean(props.key.choices?.length);
  return (
    <div class="setting-row">
      <div class="setting-copy">
        <strong class="setting-name">{settingLabel(props.key.path)}</strong>
        <p>{props.key.usage}</p>
        <div class="setting-meta">
          <code>{props.key.path}</code>
          <span>{sourceLabel(props.current?.source)}</span>
          {props.key.scope === "static" ? (
            <span class="setting-meta-scope" title={props.key.reason}>
              <Icon name="lock" /> Restart to change
            </span>
          ) : (
            <span class="setting-meta-scope">Applies live</span>
          )}
        </div>
      </div>
      <div class="setting-control">
        <div class="setting-primary-control">
          {props.key.type === "bool" ? (
            <BoolControl key={props.key} current={props.current} />
          ) : isChoice ? (
            <ChoiceControl key={props.key} current={props.current} />
          ) : (
            <ValuePreview key={props.key} value={props.current?.value} />
          )}
        </div>
        <SettingActions key={props.key} current={props.current} />
      </div>
    </div>
  );
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
            <button class="btn" type="button" data-s3key-create data-mounts={props.mountNames.join(",")}>
              <IconLabel icon="plus">Create key</IconLabel>
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
            <div class="access-key-list">
              {props.keys.map((key) => (
                <div class="access-key-row">
                  <div class="access-key-copy">
                    <code>{key.accessKey}</code>
                    <div class="access-key-meta">
                      <span>
                        Buckets <code>{key.buckets.join(", ")}</code>
                      </span>
                      <span class="muted">{key.requestsPerSecond ? `${key.requestsPerSecond} requests/s` : "No rate limit"}</span>
                    </div>
                  </div>
                  <div class="access-key-actions">
                    <form method="post" action="/settings/s3keys/toggle">
                      <input type="hidden" name="accessKey" value={key.accessKey} />
                      <SwitchControl
                        checked={!key.disabled}
                        label={`Access key ${key.accessKey}`}
                        name="disabled"
                        value={key.disabled ? "false" : "true"}
                      />
                    </form>
                    <button class="btn" type="button" data-s3key-rotate={key.accessKey}>
                      <IconLabel icon="refresh">Rotate</IconLabel>
                    </button>
                    <form method="post" action="/settings/s3keys/delete" data-confirm-s3key={key.accessKey}>
                      <input type="hidden" name="accessKey" value={key.accessKey} />
                      <button class="btn danger" type="submit">
                        <IconLabel icon="trash">Delete</IconLabel>
                      </button>
                    </form>
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>

      {sections(props.schema).map(([section, keys]) => (
        <section class="panel settings-section" aria-labelledby={`settings-${section}`}>
          <div class="panel-head settings-section-head">
            <h2 id={`settings-${section}`}>{sectionLabel(section)}</h2>
            <span class="settings-section-count">
              {keys.length} {keys.length === 1 ? "setting" : "settings"}
            </span>
          </div>
          <div class="panel-body settings-section-body">
            <div class="settings-list">
              {keys.map((key) => (
                <SettingRow key={key} current={byPath.get(key.path)} />
              ))}
            </div>
          </div>
        </section>
      ))}
      </div>
      <script type="module" src="/settings.js" />
    </Layout>
  );
}
