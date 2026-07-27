import { describe, expect, test } from "bun:test";
import type { ConfigKeySchema, ConfigValue } from "@valentinkolb/filegate";
import { renderToString } from "solid-js/web";
import { Settings } from "../src/pages/Settings";

const schema: ConfigKeySchema[] = [
  {
    path: "server.access_log_enabled",
    type: "bool",
    scope: "runtime",
    usage: "enable REST and S3 access logs",
    secret: false,
  },
  {
    path: "server.http2_cleartext",
    type: "bool",
    scope: "static",
    usage: "accept unencrypted HTTP/2",
    reason: "the listener protocol set is fixed at startup",
    secret: false,
  },
  {
    path: "detection.backend",
    type: "string",
    scope: "static",
    usage: "change detector backend",
    reason: "selects a different detector implementation",
    choices: ["auto", "poll", "btrfs"],
    secret: false,
  },
  {
    path: "versioning.enabled",
    type: "string",
    scope: "runtime",
    usage: "versioning mode",
    choices: ["auto", "on", "off"],
    secret: false,
  },
  {
    path: "server.cors.allowed_origins",
    type: "stringList",
    scope: "runtime",
    usage: "CORS allowed origins",
    secret: false,
  },
  {
    path: "versioning.retention_buckets",
    type: "retentionBuckets",
    scope: "runtime",
    usage: "version retention policy",
    secret: false,
  },
  {
    path: "auth.bearer_token",
    type: "string",
    scope: "static",
    usage: "REST bearer token",
    reason: "break-glass credential",
    secret: true,
  },
];

const values: ConfigValue[] = [
  { path: "server.access_log_enabled", value: true, source: "runtime", scope: "runtime" },
  { path: "server.http2_cleartext", value: false, source: "default", scope: "static" },
  { path: "detection.backend", value: "auto", source: "default", scope: "static" },
  { path: "versioning.enabled", value: "auto", source: "default", scope: "runtime" },
  {
    path: "server.cors.allowed_origins",
    value: ["https://admin.example", "https://files.example"],
    source: "file",
    scope: "runtime",
  },
  {
    path: "versioning.retention_buckets",
    value: [
      { keepFor: "1h0m0s", maxCount: -1 },
      { keepFor: "24h0m0s", maxCount: 4 },
    ],
    source: "runtime",
    scope: "runtime",
  },
  { path: "auth.bearer_token", value: { configured: true }, source: "env", scope: "static" },
];

function renderSettings(): string {
  return renderToString(() => (
    <Settings
      schema={schema}
      values={{ generatedAt: 1, values }}
      keys={[]}
      s3Enabled={false}
      mountNames={["data"]}
      mounts={1}
      health="ok"
    />
  ));
}

describe("typed settings controls", () => {
  test("renders runtime booleans as native submit switches", () => {
    const html = renderSettings();

    expect(html).toContain('role="switch" aria-checked="true"');
    expect(html).toContain('name="value" value="false"');
    expect(html).toContain("Access log enabled");
    expect(html).toContain("Live override");
    expect(html).toContain("Applies live");
  });

  test("renders static values without interactive affordances", () => {
    const html = renderSettings();

    expect(html.match(/role="switch"/g)).toHaveLength(1);
    expect(html).not.toContain("aria-readonly");
    expect(html).not.toContain("setting-choice-readonly");
    expect(html).toContain("HTTP/2 cleartext");
    expect(html).toContain('<strong class="setting-state-value">Off</strong>');
    expect(html).toContain('<strong class="setting-state-value is-choice">auto</strong>');
    expect(html).toContain("Restart to change");
  });

  test("uses schema choices and structured value previews", () => {
    const html = renderSettings();

    expect(html).toContain('data-setting-choice');
    expect(html).toContain('<option value="auto" selected>auto</option>');
    expect(html).toContain("https://admin.example");
    expect(html).toContain("https://files.example");
    expect(html).toContain("setting-list-value");
    expect(html).toContain("up to 4");
    expect(html).not.toContain("[object Object]");
  });

  test("renders provenance and scope as quiet metadata, not badges", () => {
    const html = renderSettings();

    expect(html).toContain("Config file");
    expect(html).toContain("Applies live");
    expect(html).not.toContain("source-default");
    expect(html).not.toContain("source-runtime");
    expect(html).not.toContain("Restart-bound");
  });

  test("shows secret presence without exposing a value editor", () => {
    const html = renderSettings();

    expect(html).toContain("Bearer token");
    expect(html).toContain("Configured");
    expect(html).not.toContain('data-setting-edit="auth.bearer_token"');
  });
});
