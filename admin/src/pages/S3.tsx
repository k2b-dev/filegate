import type {
  ActivityEvent,
  ActivityListResponse,
  ConfigKeySchema,
  ConfigValue,
  ConfigValuesResponse,
  S3Key,
} from "@valentinkolb/filegate";
import { ConfigSection } from "../components/ConfigReadout";
import { IconLabel } from "../components/Icons";
import { Layout } from "../components/Layout";
import { formatUnix } from "../lib/format";

type S3Data = {
  schema: ConfigKeySchema[];
  values: ConfigValuesResponse;
  keys: S3Key[];
  activity?: ActivityListResponse;
  mountNames: string[];
};

function ResourceSwitch(props: { checked: boolean; label: string; name: string; value: string }) {
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
      <span class="setting-switch-track" aria-hidden="true"><span class="setting-switch-knob" /></span>
      <span class="setting-switch-state">{state}</span>
    </button>
  );
}

function effective(values: ConfigValue[], path: string): unknown {
  return values.find((entry) => entry.path === path)?.effective;
}

function activityDuration(value?: number): string {
  if (!value) return "-";
  return value < 1000 ? `${Math.round(value)} ms` : `${(value / 1000).toFixed(1)} s`;
}

function averageDuration(events: ActivityEvent[]): string {
  if (events.length === 0) return "-";
  return activityDuration(events.reduce((sum, event) => sum + (event.durationMs ?? 0), 0) / events.length);
}

function operationName(operation: string): string {
  return operation.startsWith("s3.") ? operation.slice(3) : operation;
}

function targetName(event: ActivityEvent): string {
  return event.target?.path || event.target?.id || event.target?.kind || "-";
}

function MetricRow(props: { label: string; value: string; mono?: boolean }) {
  return (
    <div class="cfg-row">
      <dt>{props.label}</dt>
      <dd class={props.mono ? "mono" : undefined}>{props.value || "-"}</dd>
    </div>
  );
}

export function S3(props: S3Data & { health?: "ok" | "degraded" | "fail"; mounts: number; error?: string; notice?: string }) {
  const enabled = effective(props.values.values, "s3.enabled") === true;
  const listen = String(effective(props.values.values, "s3.listen") ?? "-");
  const region = String(effective(props.values.values, "s3.region") ?? "-");
  const activeKeys = props.keys.filter((key) => !key.disabled).length;
  const events = props.activity?.items ?? [];
  const failures = events.filter((event) => event.outcome === "failed").length;
  const latest = events[0];

  return (
    <Layout
      active="s3"
      title="S3"
      description="Listener state, access keys, recent object activity, and the effective S3 configuration."
      mounts={props.mounts}
      health={props.health}
      error={props.error}
      notice={props.notice}
    >
      <section class="summary observability-summary" style="view-transition-name: fg-s3-summary">
        <div>
          <div class="label">S3 listener</div>
          <div class={`metric small-metric${enabled ? " s3-running" : ""}`}>{enabled ? "Running" : "Disabled"}</div>
          <div class="label">{enabled ? "Accepting signed requests" : "Enable s3.enabled and restart Filegate"}</div>
        </div>
        <div>
          <div class="label">Bind</div>
          <div class="metric small-metric"><code>{listen}</code></div>
          <div class="label">Listener address</div>
        </div>
        <div>
          <div class="label">Region</div>
          <div class="metric small-metric">{region}</div>
          <div class="label">SigV4 signing region</div>
        </div>
        <div>
          <div class="label">Buckets</div>
          <div class="metric">{props.mountNames.length}</div>
          <div class="label">Mounted namespaces</div>
        </div>
        <div>
          <div class="label">Active keys</div>
          <div class="metric">{activeKeys} / {props.keys.length}</div>
          <div class="label">Enabled credentials</div>
        </div>
      </section>

      <section class="metrics-grid live-grid">
        <section class="panel" aria-labelledby="s3-endpoint-title">
          <div class="panel-head">
            <div>
              <h2 id="s3-endpoint-title">S3 endpoint</h2>
              <p>Current listener identity and available namespaces.</p>
            </div>
          </div>
          <div class="panel-body">
            <dl class="cfg">
              <MetricRow label="state" value={enabled ? "Running" : "Disabled"} />
              <MetricRow label="bind" value={listen} mono />
              <MetricRow label="region" value={region} />
              <MetricRow label="buckets" value={String(props.mountNames.length)} />
              <MetricRow label="active_keys" value={`${activeKeys} / ${props.keys.length}`} />
            </dl>
          </div>
        </section>

        <section class="panel" aria-labelledby="s3-activity-window-title">
          <div class="panel-head">
            <div>
              <h2 id="s3-activity-window-title">Activity window</h2>
              <p>Aggregate S3 object activity currently retained by Filegate.</p>
            </div>
          </div>
          <div class="panel-body">
            <dl class="cfg">
              <MetricRow label="object_operations" value={String(props.activity?.total ?? 0)} />
              <MetricRow label="failures" value={String(failures)} />
              <MetricRow label="average_duration" value={averageDuration(events)} />
              <MetricRow label="last_activity" value={latest ? formatUnix(latest.at) : "-"} />
            </dl>
            <p class="hint muted">
              Bucket listing and HEAD requests are not recorded in the retained activity window.
            </p>
          </div>
        </section>
      </section>

      <div class="page-stack">
        <section class="panel" aria-labelledby="s3-access-keys">
          <div class="panel-head">
            <div>
              <h2 id="s3-access-keys">Access keys</h2>
              <p>Keys are runtime resources and take effect immediately.</p>
            </div>
            {enabled && (
              <button class="btn" type="button" data-s3key-create data-mounts={props.mountNames.join(",")}>
                <IconLabel icon="plus">Create key</IconLabel>
              </button>
            )}
          </div>
          <div class="panel-body">
            {!enabled ? (
              <div class="empty">
                <strong>The S3 listener is disabled</strong>
                <span>Set <code>s3.enabled</code> in the manifest and restart Filegate before managing keys.</span>
              </div>
            ) : props.keys.length === 0 ? (
              <div class="empty">
                <strong>No access keys</strong>
                <span>Create a key to connect an S3 client. Its secret is shown exactly once.</span>
              </div>
            ) : (
              <div class="access-key-list">
                {props.keys.map((key) => (
                  <div class="access-key-row">
                    <div class="access-key-copy">
                      <code>{key.accessKey}</code>
                      <div class="access-key-meta">
                        <span>Buckets <code>{key.buckets.join(", ")}</code></span>
                        <span class="muted">{key.requestsPerSecond ? `${key.requestsPerSecond} requests/s` : "No rate limit"}</span>
                      </div>
                    </div>
                    <div class="access-key-actions">
                      <form method="post" action="/s3/keys/toggle">
                        <input type="hidden" name="accessKey" value={key.accessKey} />
                        <ResourceSwitch checked={!key.disabled} label={`Access key ${key.accessKey}`} name="disabled" value={key.disabled ? "false" : "true"} />
                      </form>
                      <button class="btn" type="button" data-s3key-rotate={key.accessKey}>
                        <IconLabel icon="refresh">Rotate</IconLabel>
                      </button>
                      <form method="post" action="/s3/keys/delete" data-confirm-s3key={key.accessKey}>
                        <input type="hidden" name="accessKey" value={key.accessKey} />
                        <button class="btn danger" type="submit"><IconLabel icon="trash">Delete</IconLabel></button>
                      </form>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
        </section>

        <section class="panel" aria-labelledby="s3-recent-title">
          <div class="panel-head">
            <div>
              <h2 id="s3-recent-title">Recent object activity</h2>
              <p>The latest signed reads and mutations retained by the activity ring.</p>
            </div>
          </div>
          <div class="table-wrap">
            <table class="activity-table">
              <thead>
                <tr>
                  <th>Time</th>
                  <th>Operation</th>
                  <th>Key</th>
                  <th>Object</th>
                  <th>Duration</th>
                  <th>Outcome</th>
                </tr>
              </thead>
              <tbody>
                {events.length === 0 ? (
                  <tr class="empty-row">
                    <td colSpan={6}>No S3 object activity is retained yet.</td>
                  </tr>
                ) : events.slice(0, 8).map((event) => (
                  <tr>
                    <td class="mono">{formatUnix(event.at)}</td>
                    <td class="activity-op">{operationName(event.operation)}</td>
                    <td class="mono">{event.actor.id || "-"}</td>
                    <td class="mono">{targetName(event)}</td>
                    <td>{activityDuration(event.durationMs)}</td>
                    <td><span class={`outcome outcome-${event.outcome}`}>{event.outcome}</span></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>

        <ConfigSection id="s3-configuration" title="S3 configuration" schema={props.schema} values={props.values.values} />
      </div>
      <script type="module" src="/s3.js" />
    </Layout>
  );
}
