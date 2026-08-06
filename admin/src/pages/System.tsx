import type {
  ActivityEvent,
  ActivityListResponse,
  HealthResponse,
  StatsResponse,
  SystemInfoResponse,
  SystemRuntimeResponse,
  UploadSessionSummary,
} from "@valentinkolb/filegate";
import { charts } from "@valentinkolb/stdlib";
import { Layout } from "../components/Layout";
import { env } from "../lib/env";
import { Icon, IconLabel } from "../components/Icons";
import { CachePanel, DetectorPanel, HealthPanel, LifecyclePanel, QueuePanel, UploadSessionPanel } from "./Live";
import { formatBytes, formatUnix } from "../lib/format";

type ActivityQuery = { q: string; operation: string; outcome: string; page: number; pageSize: number };

export function System(props: {
  stats: StatsResponse;
  health?: "ok" | "degraded" | "fail";
  activity?: ActivityListResponse;
  activityQuery: ActivityQuery;
  runtime?: SystemRuntimeResponse;
  info?: SystemInfoResponse;
  healthDetail?: HealthResponse;
  sessions: UploadSessionSummary[];
  canPrune?: boolean;
  error?: string;
  notice?: string;
}) {
  const cfg = env();
  const totalPages = Math.max(1, Math.ceil((props.activity?.total ?? 0) / props.activityQuery.pageSize));
  const page = Math.min(props.activityQuery.page, totalPages);
  const system = props.stats.system ?? emptySystemStats();
  const heapUsage = ratio(system.heapAllocBytes, system.heapSysBytes);
  const fdUsage = ratio(system.openFDs, system.maxFDs);
  const storageUsed = props.stats.disks.reduce((sum, disk) => sum + disk.used, 0);
  const storageSize = props.stats.disks.reduce((sum, disk) => sum + disk.size, 0);
  const storageUsage = ratio(storageUsed, storageSize);
  const activityRetained = props.activity?.retained ?? 0;
  const activityCapacity = props.activity?.capacity ?? 0;
  const activityUsage = ratio(activityRetained, activityCapacity);
  return (
    <Layout
      active="system"
      title="System"
      description="Runtime health, index state, storage pressure, and recent activity."
      mounts={props.stats.mounts.length}
      health={props.health}
      error={props.error}
      notice={props.notice}
    >
      <section class="summary observability-summary" style="view-transition-name: fg-system-summary">
        <div>
          <div class="label">Heap usage</div>
          <div class="metric">{formatPercent(heapUsage)}</div>
          <div class="label">{formatBytes(system.heapAllocBytes)} allocated</div>
        </div>
        <div>
          <div class="label">Open file descriptors</div>
          <div class="metric">{system.openFDs || "-"}</div>
          <div class="label">{system.maxFDs ? `${formatPercent(fdUsage)} of limit` : "limit unavailable"}</div>
        </div>
        <div>
          <div class="label">Index entities</div>
          <div class="metric">{formatCount(props.stats.index.totalEntities)}</div>
          <div class="label">
            {formatCount(props.stats.index.totalFiles)} files · {formatCount(props.stats.index.totalDirs)} dirs
          </div>
        </div>
        <div>
          <div class="label">Storage used</div>
          <div class="metric">{formatPercent(storageUsage)}</div>
          <div class="label">
            {formatBytes(storageUsed)} / {formatBytes(storageSize)}
          </div>
        </div>
        <div>
          <div class="label">Path cache</div>
          <div class="metric">{formatPercent(props.stats.cache.pathUtilRatio)}</div>
          <div class="label">
            {formatCount(props.stats.cache.pathEntries)} / {formatCount(props.stats.cache.pathCapacity)}
          </div>
        </div>
      </section>

      <section class="metrics-grid live-grid" data-live-root>
        <HealthPanel health={props.healthDetail} />
        <DetectorPanel runtime={props.runtime} info={props.info} />
        <QueuePanel runtime={props.runtime} />
        <CachePanel runtime={props.runtime} />
        <LifecyclePanel runtime={props.runtime} info={props.info} canPrune={props.canPrune} />
        <UploadSessionPanel runtime={props.runtime} sessions={props.sessions} />
      </section>

      <section class="metrics-grid">
        <section class="panel" style="view-transition-name: fg-system-runtime">
          <div class="panel-head">
            <div>
              <h2>Runtime pressure</h2>
              <p>Memory and garbage collection state for the running Filegate process.</p>
            </div>
          </div>
          <div class="panel-body">
            <Chart svg={charts.gauge({ width: 300, height: 180, value: heapUsage * 100, min: 0, max: 100, label: "Heap allocated", unit: "%", format: formatChartNumber, thresholds: pressureThresholds(), showNeedle: true })} />
            <dl class="cfg">
              <MetricRow label="goroutines" value={formatCount(system.goroutines)} />
              <MetricRow label="heap_alloc" value={formatBytes(system.heapAllocBytes)} />
              <MetricRow label="heap_sys" value={formatBytes(system.heapSysBytes)} />
              <MetricRow label="heap_objects" value={formatCount(system.heapObjects)} />
              <MetricRow label="gc_runs" value={formatCount(system.numGC)} />
              <MetricRow label="last_gc_pause" value={formatNanoseconds(system.lastGCPauseNs)} />
            </dl>
          </div>
        </section>

        <section class="panel" style="view-transition-name: fg-system-limits">
          <div class="panel-head">
            <div>
              <h2>Process limits</h2>
              <p>Descriptor headroom for concurrent uploads, downloads, and index work.</p>
            </div>
          </div>
          <div class="panel-body">
            <Chart svg={charts.gauge({ width: 300, height: 180, value: fdUsage * 100, min: 0, max: 100, label: "FD usage", unit: "%", format: formatChartNumber, thresholds: pressureThresholds(), showNeedle: true })} />
            <dl class="cfg">
              <MetricRow label="open_fds" value={formatCount(system.openFDs)} />
              <MetricRow label="fd_limit" value={system.maxFDs ? formatCount(system.maxFDs) : "-"} />
            </dl>
          </div>
        </section>

        <section class="panel" style="view-transition-name: fg-system-storage">
          <div class="panel-head">
            <div>
              <h2>Storage pressure</h2>
              <p>Used space by backing filesystem. High values can block uploads and snapshots.</p>
            </div>
          </div>
          <div class="panel-body">
            <Chart
              svg={charts.barGauge({
                width: 520,
                data: props.stats.disks.map((disk) => ({ label: disk.diskName || "disk", value: ratio(disk.used, disk.size) * 100, min: 0, max: 100, unit: "%" })),
                format: formatChartNumber,
                thresholds: pressureThresholds(),
              })}
            />
            <dl class="cfg">
              {props.stats.disks.map((disk) => (
                <MetricRow
                  label={disk.diskName || "disk"}
                  value={`${formatBytes(disk.used)} / ${formatBytes(disk.size)} · ${disk.fsType || "fs unknown"}`}
                />
              ))}
            </dl>
          </div>
        </section>

        <section class="panel" style="view-transition-name: fg-system-index">
          <div class="panel-head">
            <div>
              <h2>Index shape</h2>
              <p>Indexed files, directories, and Pebble database footprint.</p>
            </div>
          </div>
          <div class="panel-body">
            <Chart
              svg={charts.donut({
                width: 360,
                height: 220,
                data: [
                  { label: "Files", value: props.stats.index.totalFiles },
                  { label: "Dirs", value: props.stats.index.totalDirs },
                ],
                legend: true,
              })}
            />
            <dl class="cfg">
              <MetricRow label="generated_at" value={formatUnix(props.stats.generatedAt)} mono />
              <MetricRow label="entities" value={formatCount(props.stats.index.totalEntities)} />
              <MetricRow label="files" value={formatCount(props.stats.index.totalFiles)} />
              <MetricRow label="dirs" value={formatCount(props.stats.index.totalDirs)} />
              <MetricRow label="db_size" value={formatBytes(props.stats.index.dbSizeBytes)} />
            </dl>
          </div>
        </section>

        <section class="panel" style="view-transition-name: fg-system-cache">
          <div class="panel-head">
            <div>
              <h2>Cache pressure</h2>
              <p>Path cache occupancy. Sustained high usage points to an undersized cache.</p>
            </div>
          </div>
          <div class="panel-body">
            <Chart
              svg={charts.barGauge({
                width: 520,
                data: [{ label: "Path cache", value: props.stats.cache.pathUtilRatio * 100, min: 0, max: 100, unit: "%" }],
                format: formatChartNumber,
                thresholds: pressureThresholds(),
              })}
            />
            <dl class="cfg">
              <MetricRow label="path_entries" value={formatCount(props.stats.cache.pathEntries)} />
              <MetricRow label="path_capacity" value={formatCount(props.stats.cache.pathCapacity)} />
              <MetricRow label="path_usage" value={formatPercent(props.stats.cache.pathUtilRatio)} />
            </dl>
          </div>
        </section>

        <section class="panel" style="view-transition-name: fg-system-mounts">
          <div class="panel-head">
            <div>
              <h2>Mount distribution</h2>
              <p>Indexed entity distribution across configured mount roots.</p>
            </div>
          </div>
          <div class="panel-body">
            <Chart
              svg={charts.donut({
                width: 360,
                height: 220,
                data: props.stats.mounts.map((mount) => ({ label: mount.name, value: mount.files + mount.dirs })),
                legend: true,
              })}
            />
            <dl class="cfg">
              {props.stats.mounts.map((mount) => (
                <MetricRow label={mount.name} value={`${formatCount(mount.files)} files · ${formatCount(mount.dirs)} dirs · ${mount.path}`} />
              ))}
            </dl>
          </div>
        </section>

        <section class="panel" style="view-transition-name: fg-system-admin">
          <div class="panel-head">
            <div>
              <h2>Admin app</h2>
              <p>Connection settings used by this SSR admin process.</p>
            </div>
          </div>
          <div class="panel-body">
            <dl class="cfg">
              <MetricRow label="filegate_url" value={cfg.filegateUrl} mono />
              <MetricRow label="admin_token" value="configured; secret redacted" />
              <MetricRow label="session_secret" value="configured; secret redacted" />
            </dl>
          </div>
        </section>

        <section class="panel" style="view-transition-name: fg-system-activity-buffer">
          <div class="panel-head">
            <div>
              <h2>Activity buffer</h2>
              <p>Ring buffer usage for recent audit and admin activity events.</p>
            </div>
          </div>
          <div class="panel-body">
            <Chart
              svg={charts.barGauge({
                width: 520,
                data: [{ label: "Retained", value: activityUsage * 100, min: 0, max: 100, unit: "%" }],
                format: formatChartNumber,
                thresholds: pressureThresholds(),
              })}
            />
            <dl class="cfg">
              <MetricRow label="retained" value={`${formatCount(activityRetained)} / ${formatCount(activityCapacity)}`} />
              <MetricRow label="current_page" value={formatCount(props.activity?.items.length ?? 0)} />
              <MetricRow label="matching" value={formatCount(props.activity?.total ?? 0)} />
            </dl>
          </div>
        </section>
      </section>

      <section id="activity" class="panel activity-panel" style="view-transition-name: fg-system-activity">
        <div class="panel-head">
          <div>
            <h2>Activity</h2>
            <p>{activitySummary(props.activity, props.activityQuery)}</p>
          </div>
          <a class="btn" href={activityURL(props.activityQuery, page)}>
            <IconLabel icon="refresh">Reload activity</IconLabel>
          </a>
        </div>
        <form class="toolbar activity-filters" method="get" action="/system#activity">
          <input class="input" name="q" value={props.activityQuery.q} placeholder="Search activity" aria-label="Search activity" />
          <select class="select" name="operation" aria-label="Filter by operation">
            <option value="">All operations</option>
            {(props.activity?.operations ?? []).map((operation) => (
              <option value={operation} selected={props.activityQuery.operation === operation}>
                {operation}
              </option>
            ))}
          </select>
          <select class="select" name="outcome" aria-label="Filter by outcome">
            <option value="">All outcomes</option>
            {["succeeded", "failed", "skipped"].map((outcome) => (
              <option value={outcome} selected={props.activityQuery.outcome === outcome}>
                {outcome}
              </option>
            ))}
          </select>
          <button class="btn primary" type="submit">
            <IconLabel icon="filter">Apply</IconLabel>
          </button>
          <a class="btn" href="/system#activity">
            <IconLabel icon="filter-off">Reset</IconLabel>
          </a>
        </form>
        <div class="table-wrap">
          <table class="activity-table">
            <thead>
              <tr>
                <th>Time</th>
                <th>Operation</th>
                <th>Actor</th>
                <th>Target</th>
                <th>Duration</th>
                <th>Outcome</th>
              </tr>
            </thead>
            <tbody>
              {(props.activity?.items.length ?? 0) === 0 ? (
                <tr class="empty-row">
                  <td colSpan={6}>No activity recorded yet.</td>
                </tr>
              ) : (
                props.activity!.items.map((event) => (
                  <tr>
                    <td class="mono">{formatUnix(event.at)}</td>
                    <td class="activity-op">{event.operation}</td>
                    <td>{actorName(event)}</td>
                    <td class="mono">{targetName(event)}</td>
                    <td>{formatDuration(event.durationMs)}</td>
                    <td>
                      <span class={`outcome outcome-${event.outcome}`}>{event.outcome}</span>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
        <div class="activity-pager">
          <a class={`btn${page <= 1 ? " disabled" : ""}`} href={page <= 1 ? activityURL(props.activityQuery, 1) : activityURL(props.activityQuery, page - 1)}>
            <IconLabel icon="chevron-left">Previous</IconLabel>
          </a>
          <span>
            Page {page} of {totalPages}
          </span>
          <a
            class={`btn${page >= totalPages ? " disabled" : ""}`}
            href={page >= totalPages ? activityURL(props.activityQuery, totalPages) : activityURL(props.activityQuery, page + 1)}
          >
            Next
          </a>
        </div>
      </section>
      <script type="module" src="/system.js" />
    </Layout>
  );
}

function Chart(props: { svg: string }) {
  return <div class="metric-chart" innerHTML={props.svg} />;
}

function MetricRow(props: { label: string; value: string; mono?: boolean }) {
  return (
    <div class="cfg-row">
      <dt>{props.label}</dt>
      <dd class={props.mono ? "mono" : undefined}>{props.value || "-"}</dd>
    </div>
  );
}

function emptySystemStats() {
  return { goroutines: 0, heapAllocBytes: 0, heapSysBytes: 0, heapObjects: 0, numGC: 0, lastGCPauseNs: 0, openFDs: 0, maxFDs: 0 };
}

function actorName(event: ActivityEvent): string {
  return event.actor.delegatedActor || event.actor.label || event.actor.id || event.actor.kind;
}

function targetName(event: ActivityEvent): string {
  return event.target?.path || event.target?.id || event.target?.kind || "-";
}

function formatDuration(value?: number): string {
  if (!value) return "-";
  return value < 1000 ? `${value} ms` : `${(value / 1000).toFixed(1)} s`;
}

function ratio(value: number, total: number): number {
  if (!Number.isFinite(value) || !Number.isFinite(total) || total <= 0) return 0;
  return Math.max(0, Math.min(1, value / total));
}

function formatPercent(value: number): string {
  if (!Number.isFinite(value)) return "-";
  return `${Math.round(value * 100)}%`;
}

function formatCount(value: number): string {
  if (!Number.isFinite(value)) return "-";
  return new Intl.NumberFormat("en").format(value);
}

function formatChartNumber(value: number): string {
  return String(Math.round(value));
}

function formatNanoseconds(value: number): string {
  if (!value) return "-";
  if (value < 1_000_000) return `${Math.round(value / 1_000)} us`;
  if (value < 1_000_000_000) return `${(value / 1_000_000).toFixed(1)} ms`;
  return `${(value / 1_000_000_000).toFixed(2)} s`;
}

function pressureThresholds() {
  return [
    { value: 70, color: "#037f0c" },
    { value: 90, color: "#b35c00" },
    { value: 100, color: "#d13212" },
  ];
}

function activitySummary(activity: ActivityListResponse | undefined, query: ActivityQuery): string {
  if (!activity) return "Recent activity is unavailable.";
  const start = activity.total === 0 ? 0 : activity.offset + 1;
  const end = Math.min(activity.offset + activity.items.length, activity.total);
  const filtered = query.q || query.operation || query.outcome ? `${activity.total} matching · ` : "";
  return `${filtered}${start}-${end} shown · ${activity.retained} / ${activity.capacity} retained`;
}

function activityURL(query: ActivityQuery, page = query.page): string {
  const q = new URLSearchParams();
  if (query.q) q.set("q", query.q);
  if (query.operation) q.set("operation", query.operation);
  if (query.outcome) q.set("outcome", query.outcome);
  if (page > 1) q.set("page", String(page));
  const suffix = q.toString();
  return `/system${suffix ? `?${suffix}` : ""}#activity`;
}
