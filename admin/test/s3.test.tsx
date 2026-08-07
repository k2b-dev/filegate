import { describe, expect, test } from "bun:test";
import type { ActivityListResponse, ConfigKeySchema, ConfigValue, S3Key } from "@k2b/filegate";
import { renderToString } from "solid-js/web";
import { S3 } from "../src/pages/S3";

const schema: ConfigKeySchema[] = [
  {
    path: "s3.enabled",
    type: "bool",
    scope: "static",
    managedBy: "manifest",
    usage: "enable S3-compatible listener",
    reason: "controls whether the second listener exists",
    secret: false,
  },
  {
    path: "s3.listen",
    type: "string",
    scope: "static",
    managedBy: "manifest",
    usage: "S3 listener address",
    reason: "S3 listener bind address",
    secret: false,
  },
  {
    path: "s3.region",
    type: "string",
    scope: "static",
    managedBy: "manifest",
    usage: "S3 SigV4 region",
    reason: "the signing handler captures its region at startup",
    secret: false,
  },
];

const values: ConfigValue[] = [
  { path: "s3.enabled", effective: true, desired: true, source: "manifest", scope: "static", managedBy: "manifest" },
  { path: "s3.listen", effective: ":9000", desired: ":9000", source: "env", scope: "static", managedBy: "manifest" },
  { path: "s3.region", effective: "us-east-1", desired: "eu-central-1", source: "manifest", scope: "static", managedBy: "manifest" },
];

const keys: S3Key[] = [
  {
    accessKey: "SYNC",
    buckets: ["data"],
    requestsPerSecond: 25,
    burst: 25,
    disabled: false,
  },
];

const activity: ActivityListResponse = {
  items: [
    {
      id: "1",
      at: 1720000000000,
      actor: { kind: "s3_key", id: "SYNC" },
      operation: "s3.PutObject",
      outcome: "succeeded",
      target: { kind: "s3_object", path: "data/photo.jpg" },
      durationMs: 12,
    },
    {
      id: "2",
      at: 1719999999000,
      actor: { kind: "s3_key", id: "SYNC" },
      operation: "s3.GetObject",
      outcome: "failed",
      target: { kind: "s3_object", path: "data/missing.jpg" },
      durationMs: 4,
    },
  ],
  total: 2,
  offset: 0,
  limit: 2,
  retained: 8,
  capacity: 500,
  operations: ["s3.GetObject", "s3.PutObject"],
};

function renderS3(enabled = true): string {
  const pageValues = values.map((value) => value.path === "s3.enabled" ? { ...value, effective: enabled, desired: enabled } : value);
  return renderToString(() => (
    <S3
      schema={schema}
      values={{ generatedAt: 1, values: pageValues }}
      keys={enabled ? keys : []}
      activity={activity}
      mountNames={["data"]}
      mounts={1}
      health="ok"
    />
  ));
}

describe("S3 administration", () => {
  test("shows the listener identity and retained object statistics", () => {
    const html = renderS3();

    expect(html).toContain("S3 listener");
    expect(html).toContain(":9000");
    expect(html).toContain("Running");
    expect(html).toContain("object_operations");
    expect(html).toContain("retained activity window");
    expect(html).toContain("PutObject");
    expect(html).toContain("data/photo.jpg");
    expect(html).toContain('class="summary observability-summary"');
    expect(html).toContain('class="metrics-grid live-grid"');
    expect(html).not.toContain("s3-listener-main");
    expect(html).not.toContain("s3-activity-ledger");
  });

  test("keeps access-key controls on S3 routes", () => {
    const html = renderS3();

    expect(html).toContain("Access keys");
    expect(html).toContain("SYNC");
    expect(html).toContain("/s3/keys/toggle");
    expect(html).toContain("/s3/keys/delete");
    expect(html).not.toContain("/settings/s3keys");
  });

  test("renders all S3 manifest values as read-only configuration", () => {
    const html = renderS3();

    expect(html).toContain("S3 configuration");
    expect(html).toContain("s3.region");
    expect(html).toContain("After restart");
    expect(html).toContain("eu-central-1");
  });

  test("explains the disabled state without exposing key actions", () => {
    const html = renderS3(false);

    expect(html).toContain("Disabled");
    expect(html).toContain("Enable s3.enabled and restart Filegate");
    expect(html).not.toContain("data-s3key-create");
    expect(html).not.toContain('role="switch"');
  });
});
