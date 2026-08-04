import { describe, expect, test } from "bun:test";
import type { ConfigKeySchema, ConfigValue } from "@valentinkolb/filegate";
import { renderToString } from "solid-js/web";
import { Settings } from "../src/pages/Settings";

const schema: ConfigKeySchema[] = [
  {
    path: "server.access_log_enabled",
    type: "bool",
    scope: "runtime",
    managedBy: "manifest",
    usage: "enable REST and S3 access logs",
    secret: false,
  },
  {
    path: "server.http2_cleartext",
    type: "bool",
    scope: "static",
    managedBy: "manifest",
    usage: "accept unencrypted HTTP/2",
    reason: "the listener protocol set is fixed at startup",
    secret: false,
  },
  {
    path: "versioning.retention_buckets",
    type: "retentionBuckets",
    scope: "runtime",
    managedBy: "manifest",
    usage: "version retention policy",
    secret: false,
  },
  {
    path: "auth.bearer_token",
    type: "string",
    scope: "static",
    managedBy: "bootstrap",
    usage: "REST bearer token",
    reason: "break-glass credential",
    secret: true,
  },
];

const values: ConfigValue[] = [
  {
    path: "server.access_log_enabled",
    effective: true,
    desired: true,
    source: "manifest",
    scope: "runtime",
    managedBy: "manifest",
  },
  {
    path: "server.http2_cleartext",
    effective: false,
    desired: true,
    source: "manifest",
    scope: "static",
    managedBy: "manifest",
  },
  {
    path: "versioning.retention_buckets",
    effective: [
      { keepFor: "1h0m0s", maxCount: -1 },
      { keepFor: "24h0m0s", maxCount: 4 },
    ],
    desired: [
      { keepFor: "1h0m0s", maxCount: -1 },
      { keepFor: "24h0m0s", maxCount: 4 },
    ],
    source: "manifest",
    scope: "runtime",
    managedBy: "manifest",
  },
  {
    path: "auth.bearer_token",
    effective: { configured: true },
    desired: { configured: true },
    source: "env",
    scope: "static",
    managedBy: "bootstrap",
  },
];

function renderSettings(): string {
  return renderToString(() => (
    <Settings
      schema={schema}
      values={{
        generatedAt: 1,
        manifest: { revision: "0123456789abcdef", appliedAt: 1720000000000, appliedBy: "alice" },
        values,
        restartRequired: [{ path: "server.http2_cleartext", effective: "false", desired: "true" }],
      }}
      mounts={1}
      health="ok"
    />
  ));
}

describe("read-only settings", () => {
  test("shows manifest identity and apply workflow", () => {
    const html = renderSettings();

    expect(html).toContain("Configuration manifest");
    expect(html).toContain("0123456789ab");
    expect(html).toContain("alice");
    expect(html).toContain("filegate config plan");
    expect(html).toContain("filegate config apply");
  });

  test("renders config values without mutation controls", () => {
    const html = renderSettings();

    expect(html).toContain("Access log enabled");
    expect(html).toContain("Applied manifest");
    expect(html).toContain("Applies live");
    expect(html).not.toContain("/settings/apply");
    expect(html).not.toContain("/settings/reset");
    expect(html).not.toContain("data-setting-edit");
    expect(html).not.toContain("data-setting-choice");
    expect(html).not.toContain('role="switch"');
    expect(html).not.toContain("S3 access keys");
    expect(html).not.toContain("s3.");
  });

  test("shows desired and effective values for static changes", () => {
    const html = renderSettings();

    expect(html).toContain("Restart required");
    expect(html).toContain("HTTP/2 cleartext");
    expect(html).toContain("After restart");
    expect(html).toContain("false → true");
  });

  test("renders structured values and secret presence clearly", () => {
    const html = renderSettings();

    expect(html).toContain("keep all within");
    expect(html).toContain("keep 4 within");
    expect(html).not.toContain("[object Object]");
    expect(html).toContain("Bearer token");
    expect(html).toContain("Configured");
    expect(html).toContain("Bootstrap-owned");
  });
});
