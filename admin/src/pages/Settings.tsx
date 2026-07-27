import type { ConfigKeySchema, ConfigValuesResponse } from "@valentinkolb/filegate";
import { ConfigSection, configSections, sectionLabel } from "../components/ConfigReadout";
import { Layout } from "../components/Layout";

type SettingsData = {
  schema: ConfigKeySchema[];
  values: ConfigValuesResponse;
};

function manifestTime(timestamp: number): string {
  if (!timestamp) return "-";
  return new Date(timestamp).toISOString().replace("T", " ").replace(".000Z", " UTC");
}

export function Settings(props: SettingsData & { health?: "ok" | "degraded" | "fail"; mounts: number; error?: string; notice?: string }) {
  const restarts = props.values.restartRequired ?? [];
  const manifest = props.values.manifest;

  return (
    <Layout
      active="settings"
      title="Settings"
      description="The running configuration and the declarative state applied by your deployment."
      mounts={props.mounts}
      health={props.health}
      error={props.error}
      notice={props.notice}
    >
      <div class="page-stack">
        <div class="panel manifest-summary">
          <div class="panel-head"><h2>Configuration manifest</h2></div>
          <div class="panel-body">
            {manifest ? (
              <dl class="manifest-details">
                <div><dt>Revision</dt><dd><code title={manifest.revision}>{manifest.revision.slice(0, 12)}</code></dd></div>
                <div><dt>Applied</dt><dd>{manifestTime(manifest.appliedAt)}</dd></div>
                <div><dt>Actor</dt><dd>{manifest.appliedBy}</dd></div>
              </dl>
            ) : (
              <p class="muted">No manifest has been applied. Filegate is running from bootstrap sources and built-in defaults.</p>
            )}
            <p class="manifest-help">Changes are planned and applied with <code>filegate config plan</code> and <code>filegate config apply</code>.</p>
          </div>
        </div>

        {restarts.length > 0 && (
          <div class="panel restart-banner">
            <div class="panel-head"><h2>Restart required</h2></div>
            <div class="panel-body">
              <p class="muted">The desired manifest is stored, but these static values still use the running configuration.</p>
              <ul class="restart-list">
                {restarts.map((entry) => (
                  <li>
                    <code>{entry.path}</code>
                    <span class="muted">{entry.effective || "(empty)"} → {entry.desired || "(empty)"}</span>
                  </li>
                ))}
              </ul>
            </div>
          </div>
        )}

        {configSections(props.schema).map(([section, keys]) => (
          <ConfigSection
            id={`settings-${section}`}
            title={sectionLabel(section)}
            schema={keys}
            values={props.values.values}
          />
        ))}
      </div>
    </Layout>
  );
}
