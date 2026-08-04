import { text } from "@valentinkolb/stdlib";

/**
 * Keeps the operational panels current.
 *
 * Polling rather than SSE: the payload is a single small JSON document, the
 * server holds no per-client state, and a dropped connection needs no recovery
 * path. An SSE stream would add a long-lived connection per open tab for values
 * that change on a seconds timescale anyway.
 *
 * The first frame is server-rendered, so the page is complete without any of
 * this; the poll only keeps it fresh.
 */
const intervalMs = 5000;

type Runtime = {
  detector: {
    backend: string;
    intervalMs: number;
    cycles: number;
    lastScanDurationMs: number;
    staleForMs: number;
    errors: number;
    pendingBatches: number;
    queueCapacity: number;
    trackedDirs?: number;
    trackedFiles?: number;
  };
  jobs: { workers: number; queued: number; queueCapacity: number; inFlight: number; rejected: number; panics: number };
  pathCache: { entries: number; capacity: number; hitRatio: number };
  thumbnailCache: { entries: number; capacity: number; hitRatio: number };
  uploadSessions: { inProgress: number; committing: number; aborted: number; writeSlotsInUse: number; writeSlotsLimit: number };
  lifecycle: { lastPruneAt: number; lastPruneDurationMs: number; nextPruneAt: number; filesScanned: number; versionsKept: number; versionsDeleted: number; orphansPurged: number; blobsDeleted: number };
};

type Payload = { runtime?: Runtime; health?: { status: string }; error?: string };

function ms(value: number): string {
  if (value <= 0) return "-";
  return text.pprintDurationMs(value);
}

function ratio(part: number, whole: number): string {
  if (whole <= 0) return "0%";
  return `${Math.round((part / whole) * 100)}%`;
}

/** Values keyed by the data-live path they patch. */
function fields(payload: Payload): Record<string, string> {
  const r = payload.runtime;
  if (!r) return {};
  const d = r.detector;
  const stale = d.staleForMs > d.intervalMs * 5;

  return {
    "health.status": payload.health?.status ?? "unknown",
    "detector.state": stale ? "stalled" : "keeping up",
    "detector.backend": d.backend,
    "detector.intervalMs": ms(d.intervalMs),
    "detector.staleForMs": `${ms(d.staleForMs)} ago`,
    "detector.lastScanDurationMs": ms(d.lastScanDurationMs),
    "detector.cycles": text.pprintNumber(d.cycles),
    "detector.errors": text.pprintNumber(d.errors),
    "detector.pendingBatches": `${d.pendingBatches} / ${d.queueCapacity}`,
    "detector.tracked": `${text.pprintNumber(d.trackedFiles ?? 0)} files, ${text.pprintNumber(d.trackedDirs ?? 0)} dirs`,
    "jobs.queued": `${r.jobs.queued} / ${r.jobs.queueCapacity} (${ratio(r.jobs.queued, r.jobs.queueCapacity)})`,
    "jobs.workers": `${r.jobs.inFlight} busy of ${r.jobs.workers}`,
    "jobs.rejected": text.pprintNumber(r.jobs.rejected),
    "jobs.panics": text.pprintNumber(r.jobs.panics),
    "uploads.writeSlots": `${r.uploadSessions.writeSlotsInUse} / ${r.uploadSessions.writeSlotsLimit}`,
    "uploads.counts": `${r.uploadSessions.inProgress} in progress, ${r.uploadSessions.committing} committing, ${r.uploadSessions.aborted} aborted`,
    "pathCache.hitRatio": `${Math.round(r.pathCache.hitRatio * 100)}% hit, ${text.pprintNumber(r.pathCache.entries)} / ${text.pprintNumber(r.pathCache.capacity)} entries`,
    "thumbnailCache.hitRatio": `${Math.round(r.thumbnailCache.hitRatio * 100)}% hit, ${text.pprintNumber(r.thumbnailCache.entries)} / ${text.pprintNumber(r.thumbnailCache.capacity)} entries`,
    "lifecycle.lastPruneDurationMs": ms(r.lifecycle.lastPruneDurationMs),
    "lifecycle.filesScanned": text.pprintNumber(r.lifecycle.filesScanned),
    "lifecycle.versionsKept": text.pprintNumber(r.lifecycle.versionsKept),
    "lifecycle.versionsDeleted": text.pprintNumber(r.lifecycle.versionsDeleted),
    "lifecycle.orphansPurged": text.pprintNumber(r.lifecycle.orphansPurged),
    "lifecycle.blobsDeleted": text.pprintNumber(r.lifecycle.blobsDeleted),
  };
}

function applyStatusClasses(payload: Payload): void {
  const health = document.querySelector<HTMLElement>('[data-live="health.status"]');
  if (health) health.className = `tag ${payload.health?.status === "ok" ? "pin" : "warn"}`;

  const detector = document.querySelector<HTMLElement>('[data-live="detector.state"]');
  const d = payload.runtime?.detector;
  if (detector && d) detector.className = `tag ${d.staleForMs > d.intervalMs * 5 ? "warn" : "pin"}`;
}

let failures = 0;

async function tick(root: HTMLElement): Promise<void> {
  try {
    const res = await fetch("/api/runtime", { credentials: "same-origin" });
    if (!res.ok) throw new Error(`runtime endpoint answered ${res.status}`);
    const payload = (await res.json()) as Payload;

    for (const [path, value] of Object.entries(fields(payload))) {
      const target = document.querySelector<HTMLElement>(`[data-live="${path}"]`);
      // Only touch what changed, so a value being read is not reflowed under
      // the cursor every five seconds.
      if (target && target.textContent !== value) target.textContent = value;
    }
    applyStatusClasses(payload);

    failures = 0;
    root.dataset.liveState = "live";
  } catch {
    failures++;
    // Say so rather than showing values that quietly stopped updating; three
    // misses is roughly fifteen seconds, past a transient hiccup.
    if (failures >= 3) root.dataset.liveState = "stale";
  }
}

const root = document.querySelector<HTMLElement>("[data-live-root]");
if (root) {
  window.setInterval(() => void tick(root), intervalMs);
  // A backgrounded tab stops firing timers reliably; refresh on return so the
  // first thing seen is current rather than minutes old.
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") void tick(root);
  });
}
