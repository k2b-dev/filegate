import type { ConfigKeySchema, ConfigValue } from "@valentinkolb/filegate";
import { text } from "@valentinkolb/stdlib";
import { Icon } from "./Icons";

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
  return segment.split("_").map((word) => acronyms[word] ?? word).join(" ");
}

function sentence(segment: string): string {
  const label = words(segment);
  return label.charAt(0).toUpperCase() + label.slice(1);
}

function settingLabel(path: string): string {
  const parts = path.split(".").slice(1);
  const leaf = sentence(parts.at(-1) ?? path);
  return parts.length < 2 ? leaf : `${sentence(parts.at(-2) ?? "")} · ${leaf}`;
}

export function sectionLabel(section: string): string {
  return acronyms[section] ?? sentence(section);
}

export function configSections(schema: ConfigKeySchema[]): [string, ConfigKeySchema[]][] {
  const grouped = new Map<string, ConfigKeySchema[]>();
  for (const key of schema) {
    const section = key.path.split(".")[0] ?? "other";
    grouped.set(section, [...(grouped.get(section) ?? []), key]);
  }
  return [...grouped.entries()].sort(([a], [b]) => a.localeCompare(b));
}

export function displayConfigValue(value: unknown, key?: ConfigKeySchema): string {
  if (value === null || value === undefined) return "-";
  if (typeof value === "object" && !Array.isArray(value) && "configured" in (value as Record<string, unknown>)) {
    return (value as { configured: boolean }).configured ? "Configured" : "Not configured";
  }
  if (typeof value === "boolean") return value ? "On" : "Off";
  if (key?.type === "retentionBuckets") return describeRetention(value);
  if (Array.isArray(value)) return value.length ? value.map(String).join(", ") : "None";
  if (key?.unit === "bytes" && typeof value === "number") return value > 0 ? text.pprintBytes(value) : "0";
  if (key?.type === "duration") return formatDurationValue(value);
  if (typeof value === "number") {
    const count = text.pprintNumber(value);
    return key?.unit ? `${count} ${key.unit}` : count;
  }
  return String(value);
}

function formatDurationValue(value: unknown): string {
  const raw = String(value);
  const match = /^(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)s)?$/.exec(raw);
  if (!match) return raw;
  const total = (Number(match[1] ?? 0) * 3600 + Number(match[2] ?? 0) * 60 + Number(match[3] ?? 0)) * 1000;
  return total > 0 ? text.pprintDurationMs(total) : raw;
}

type Bucket = { keepFor?: string; maxCount?: number };

function describeRetention(value: unknown): string {
  if (!Array.isArray(value) || value.length === 0) return "Versions accumulate";
  return value
    .map((entry) => {
      const bucket = entry as Bucket;
      const window = bucket.keepFor ? formatDurationValue(bucket.keepFor) : "?";
      return bucket.maxCount === undefined || bucket.maxCount < 0
        ? `keep all within ${window}`
        : `keep ${bucket.maxCount} within ${window}`;
    })
    .join(" · ");
}

function sourceLabel(source: ConfigValue["source"] | undefined): string {
  switch (source) {
    case "env":
      return "Environment";
    case "file":
      return "Bootstrap file";
    case "manifest":
      return "Applied manifest";
    default:
      return "Built-in default";
  }
}

function ownerLabel(key: ConfigKeySchema): string {
  switch (key.managedBy) {
    case "bootstrap":
      return "Bootstrap-owned";
    case "resource":
      return "Managed as a resource";
    default:
      return key.scope === "runtime" ? "Applies live" : "Applies on restart";
  }
}

function sameValue(left: unknown, right: unknown): boolean {
  return JSON.stringify(left) === JSON.stringify(right);
}

function ValuePreview(props: { schemaKey: ConfigKeySchema; value: unknown }) {
  const configured =
    typeof props.value === "object" && props.value !== null && !Array.isArray(props.value) && "configured" in props.value
      ? Boolean((props.value as { configured?: unknown }).configured)
      : undefined;
  if (configured !== undefined) {
    return (
      <span class={`setting-secret-state ${configured ? "is-configured" : ""}`}>
        <Icon name={configured ? "lock-check" : "lock-open"} />
        {configured ? "Configured" : "Not configured"}
      </span>
    );
  }
  return <strong class="setting-value-text">{displayConfigValue(props.value, props.schemaKey)}</strong>;
}

function SettingRow(props: { schemaKey: ConfigKeySchema; current?: ConfigValue }) {
  const pending = props.current && !sameValue(props.current.effective, props.current.desired);
  return (
    <div class="setting-row">
      <div class="setting-copy">
        <strong class="setting-name">{settingLabel(props.schemaKey.path)}</strong>
        <p>{props.schemaKey.usage}</p>
        <div class="setting-meta">
          <code>{props.schemaKey.path}</code>
          <span>{sourceLabel(props.current?.source)}</span>
          <span title={props.schemaKey.reason}>{ownerLabel(props.schemaKey)}</span>
        </div>
      </div>
      <div class="setting-readout">
        <ValuePreview schemaKey={props.schemaKey} value={props.current?.effective} />
        {pending && (
          <span class="setting-desired">
            After restart <strong>{displayConfigValue(props.current?.desired, props.schemaKey)}</strong>
          </span>
        )}
      </div>
    </div>
  );
}

export function ConfigSection(props: { id: string; title: string; schema: ConfigKeySchema[]; values: ConfigValue[] }) {
  const byPath = new Map<string, ConfigValue>(props.values.map((value) => [value.path, value]));
  return (
    <section class="panel settings-section" aria-labelledby={props.id}>
      <div class="panel-head settings-section-head">
        <h2 id={props.id}>{props.title}</h2>
        <span class="settings-section-count">
          {props.schema.length} {props.schema.length === 1 ? "setting" : "settings"}
        </span>
      </div>
      <div class="panel-body settings-section-body">
        <div class="settings-list">
          {props.schema.map((key) => <SettingRow schemaKey={key} current={byPath.get(key.path)} />)}
        </div>
      </div>
    </section>
  );
}
