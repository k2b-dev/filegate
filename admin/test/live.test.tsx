import { describe, expect, test } from "bun:test";
import type { SystemInfoResponse, SystemRuntimeResponse } from "@valentinkolb/filegate";
import { renderToString } from "solid-js/web";
import { DetectorPanel, LifecyclePanel } from "../src/pages/Live";

const info = {
  detector: {
    configuredBackend: "auto",
    backend: "poll",
    reason: "auto selected poll because the btrfs CLI is unavailable",
    intervalMs: 3000,
    reconcileIntervalMs: 86_400_000,
  },
  versioning: {
    enabled: true,
    mode: "on",
    copyMode: "mixed",
    reason: "enabled by configuration",
  },
  mounts: [
    { reflinkSupported: true },
    { reflinkSupported: false },
  ],
} as SystemInfoResponse;

const runtime = {
  detector: {
    backend: "poll",
    intervalMs: 3000,
    staleForMs: 500,
    lastScanDurationMs: 12,
    cycles: 7,
    errors: 0,
    pendingBatches: 0,
    queueCapacity: 64,
    trackedFiles: 10,
    trackedDirs: 3,
  },
  lifecycle: {
    prunerIntervalMs: 300_000,
    lastPruneAt: 0,
  },
} as SystemRuntimeResponse;

describe("effective storage modes", () => {
  test("renders configured and effective detector state", () => {
    const html = renderToString(() => <DetectorPanel runtime={runtime} info={info} />);
    expect(html).toContain("Configured");
    expect(html).toContain("auto");
    expect(html).toContain("poll");
    expect(html).toContain("btrfs CLI is unavailable");
    expect(html).toContain("every 1d");
  });

  test("renders versioning copy mode and reflink mount count", () => {
    const html = renderToString(() => <LifecyclePanel runtime={runtime} info={info} canPrune />);
    expect(html).toContain("mixed");
    expect(html).toContain("enabled by configuration");
    expect(html).toContain("1 / 2");
  });
});
