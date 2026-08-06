import type { HealthResponse, SystemInfoResponse, SystemRuntimeResponse, UploadSessionSummary } from "@valentinkolb/filegate";
import { text } from "@valentinkolb/stdlib";
import { formatBytes, formatUnix } from "../lib/format";
import { IconLabel } from "../components/Icons";

/**
 * Live operational panels.
 *
 * Values carry data-live paths so the polling script in system.ts can patch
 * them in place. Server-rendering the first frame means the page is complete
 * without JavaScript and the poll only keeps it current.
 */

function ratio(part: number, whole: number): string {
  if (whole <= 0) return "0%";
  return `${Math.round((part / whole) * 100)}%`;
}

function ms(value: number): string {
  if (value <= 0) return "-";
  return text.pprintDurationMs(value);
}

export function HealthPanel(props: { health?: HealthResponse }) {
  const checks = props.health?.checks ?? [];
  return (
    <div class="panel">
      <div class="panel-head">
        <h2>Service health</h2>
        <span class={`tag ${props.health?.status === "ok" ? "pin" : "warn"}`} data-live="health.status">
          {props.health?.status ?? "unknown"}
        </span>
      </div>
      <div class="panel-body">
        {checks.length === 0 ? (
          <p class="muted">Health could not be read.</p>
        ) : (
          <ul class="check-list" data-live-list="health.checks">
            {checks.map((check) => (
              <li>
                <span class={`dot dot-${check.status}`} aria-hidden="true" />
                <strong>{check.name}</strong>
                <span class="muted">{check.detail || check.status}</span>
              </li>
            ))}
          </ul>
        )}
        <p class="muted hint">
          A stalled detector is the quiet failure: writes made outside the API stop being indexed without any request
          failing.
        </p>
      </div>
    </div>
  );
}

export function DetectorPanel(props: { runtime?: SystemRuntimeResponse; info?: SystemInfoResponse }) {
  const d = props.runtime?.detector;
  const info = props.info?.detector;
  const stale = d ? d.staleForMs > d.intervalMs * 5 : false;
  return (
    <div class="panel">
      <div class="panel-head">
        <h2>Change detection</h2>
        <span class={`tag ${stale ? "warn" : "pin"}`} data-live="detector.state">
          {d ? (stale ? "stalled" : "keeping up") : "unknown"}
        </span>
      </div>
      <div class="panel-body">
        <dl class="kv">
          <dt>Configured</dt>
          <dd>{info?.configuredBackend ?? "-"}</dd>
          <dt>Backend</dt>
          <dd data-live="detector.backend">{d?.backend ?? "-"}</dd>
          <dt>Selection</dt>
          <dd>{info?.reason || "-"}</dd>
          <dt>Scan interval</dt>
          <dd data-live="detector.intervalMs">{d ? ms(d.intervalMs) : "-"}</dd>
          <dt>Full reconciliation</dt>
          <dd>{info ? (info.reconcileIntervalMs > 0 ? `every ${ms(info.reconcileIntervalMs)}` : "disabled") : "-"}</dd>
          <dt>Last scan</dt>
          <dd data-live="detector.staleForMs">{d ? `${ms(d.staleForMs)} ago` : "-"}</dd>
          <dt>Scan duration</dt>
          <dd data-live="detector.lastScanDurationMs">{d ? ms(d.lastScanDurationMs) : "-"}</dd>
          <dt>Rounds</dt>
          <dd data-live="detector.cycles">{d?.cycles ?? "-"}</dd>
          <dt>Scan errors</dt>
          <dd data-live="detector.errors">{d?.errors ?? "-"}</dd>
          <dt>Pending batches</dt>
          <dd data-live="detector.pendingBatches">
            {d ? `${d.pendingBatches} / ${d.queueCapacity}` : "-"}
          </dd>
          {d?.trackedFiles !== undefined && (
            <>
              <dt>Tracked</dt>
              <dd data-live="detector.tracked">
                {d.trackedFiles} files, {d.trackedDirs ?? 0} dirs
              </dd>
            </>
          )}
        </dl>
      </div>
    </div>
  );
}

export function QueuePanel(props: { runtime?: SystemRuntimeResponse }) {
  const jobs = props.runtime?.jobs;
  const uploads = props.runtime?.uploadSessions;
  return (
    <div class="panel">
      <div class="panel-head">
        <h2>Saturation</h2>
      </div>
      <div class="panel-body">
        <dl class="kv">
          <dt>Job queue</dt>
          <dd data-live="jobs.queued">
            {jobs ? `${jobs.queued} / ${jobs.queueCapacity} (${ratio(jobs.queued, jobs.queueCapacity)})` : "-"}
          </dd>
          <dt>Workers</dt>
          <dd data-live="jobs.workers">{jobs ? `${jobs.inFlight} busy of ${jobs.workers}` : "-"}</dd>
          <dt>Rejected</dt>
          <dd data-live="jobs.rejected">{jobs?.rejected ?? "-"}</dd>
          <dt>Job panics</dt>
          <dd data-live="jobs.panics">{jobs?.panics ?? "-"}</dd>
          <dt>Upload write slots</dt>
          <dd data-live="uploads.writeSlots">
            {uploads ? `${uploads.writeSlotsInUse} / ${uploads.writeSlotsLimit}` : "-"}
          </dd>
        </dl>
        <p class="muted hint">A full job queue is what makes the thumbnail endpoint answer 503.</p>
      </div>
    </div>
  );
}

export function CachePanel(props: { runtime?: SystemRuntimeResponse }) {
  const path = props.runtime?.pathCache;
  const thumb = props.runtime?.thumbnailCache;
  return (
    <div class="panel">
      <div class="panel-head">
        <h2>Cache effectiveness</h2>
      </div>
      <div class="panel-body">
        <dl class="kv">
          <dt>Path cache</dt>
          <dd data-live="pathCache.hitRatio">
            {path ? `${Math.round(path.hitRatio * 100)}% hit, ${path.entries} / ${path.capacity} entries` : "-"}
          </dd>
          <dt>Thumbnail cache</dt>
          <dd data-live="thumbnailCache.hitRatio">
            {thumb ? `${Math.round(thumb.hitRatio * 100)}% hit, ${thumb.entries} / ${thumb.capacity} entries` : "-"}
          </dd>
        </dl>
        <p class="muted hint">Occupancy alone cannot tell an undersized cache from a cold one; the hit ratio can.</p>
      </div>
    </div>
  );
}

export function LifecyclePanel(props: { runtime?: SystemRuntimeResponse; info?: SystemInfoResponse; canPrune?: boolean }) {
  const l = props.runtime?.lifecycle;
  const versioning = props.info?.versioning;
  const mounts = props.info?.mounts ?? [];
  const reflinkMounts = mounts.filter((mount) => mount.reflinkSupported).length;
  const ran = !!l && l.lastPruneAt > 0;
  return (
    <div class="panel">
      <div class="panel-head">
        <h2>Version retention</h2>
        <span class="tb-group">
          {l?.pruneError && <span class="tag warn">last run failed</span>}
          {props.canPrune && (
            <form method="post" action="/system/prune" data-confirm-prune>
              <button class="btn" type="submit">
                <IconLabel icon="trash-x">Prune now</IconLabel>
              </button>
            </form>
          )}
        </span>
      </div>
      <div class="panel-body">
        <dl class="kv">
          <dt>Configured</dt>
          <dd>{versioning?.mode ?? "-"}</dd>
          <dt>Effective</dt>
          <dd>{versioning ? (versioning.enabled ? "enabled" : "disabled") : "-"}</dd>
          <dt>Copy mode</dt>
          <dd>{versioning?.copyMode ?? "-"}</dd>
          <dt>Selection</dt>
          <dd>{versioning?.reason || "-"}</dd>
          <dt>Reflink mounts</dt>
          <dd>{mounts.length > 0 ? `${reflinkMounts} / ${mounts.length}` : "-"}</dd>
        </dl>
        {!ran ? (
          <p class="muted">
            {l && l.prunerIntervalMs > 0
              ? `No pruning round has completed yet. The pruner runs every ${ms(l.prunerIntervalMs)}.`
              : "Versioning is disabled, so nothing is pruned."}
          </p>
        ) : (
          <dl class="kv">
            <dt>Last run</dt>
            <dd data-live="lifecycle.lastPruneAt">{formatUnix(l.lastPruneAt)}</dd>
            <dt>Duration</dt>
            <dd data-live="lifecycle.lastPruneDurationMs">{ms(l.lastPruneDurationMs)}</dd>
            <dt>Next run</dt>
            <dd data-live="lifecycle.nextPruneAt">{formatUnix(l.nextPruneAt)}</dd>
            <dt>Files scanned</dt>
            <dd data-live="lifecycle.filesScanned">{l.filesScanned}</dd>
            <dt>Versions kept</dt>
            <dd data-live="lifecycle.versionsKept">{l.versionsKept}</dd>
            <dt>Versions deleted</dt>
            <dd data-live="lifecycle.versionsDeleted">{l.versionsDeleted}</dd>
            <dt>Orphans purged</dt>
            <dd data-live="lifecycle.orphansPurged">{l.orphansPurged}</dd>
            <dt>Blobs deleted</dt>
            <dd data-live="lifecycle.blobsDeleted">{l.blobsDeleted}</dd>
          </dl>
        )}
        {l?.pruneError && <div class="error">{l.pruneError}</div>}
      </div>
    </div>
  );
}

/**
 * Upload sessions, with an abort for orphans.
 *
 * An interrupted browser upload leaves a session holding staged bytes until the
 * cleanup loop notices. This is where an operator finds and releases them.
 */
export function UploadSessionPanel(props: { runtime?: SystemRuntimeResponse; sessions: UploadSessionSummary[] }) {
  const counts = props.runtime?.uploadSessions;
  const stale = props.sessions.filter((session) => session.phase === "in_progress");

  return (
    <div class="panel">
      <div class="panel-head">
        <h2>Upload sessions</h2>
        <span class="muted" data-live="uploads.counts">
          {counts
            ? `${counts.inProgress} in progress, ${counts.committing} committing, ${counts.aborted} aborted`
            : "-"}
        </span>
      </div>
      <div class="panel-body">
        {stale.length === 0 ? (
          <p class="muted">No sessions in progress.</p>
        ) : (
          <div class="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Path</th>
                  <th class="num">Uploaded</th>
                  <th>Age</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {stale.map((session) => (
                  <tr>
                    <td>
                      <code>{session.path}</code>
                    </td>
                    <td class="num">
                      {session.uploadedSegments} / {session.totalSegments} ({formatBytes(session.uploadedBytes)})
                    </td>
                    <td class="muted">{ms(session.ageMs)}</td>
                    <td class="row-actions">
                      <span class="actions">
                        <form method="post" action="/system/sessions/abort" data-confirm-session={session.path}>
                          <input type="hidden" name="sessionId" value={session.id} />
                          <button class="btn danger" type="submit">
                            <IconLabel icon="player-stop">Abort</IconLabel>
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
  );
}
