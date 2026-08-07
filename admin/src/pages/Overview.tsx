import type { Node, StatsResponse } from "@k2b/filegate";
import { Layout } from "../components/Layout";
import { NodeTable } from "../components/Table";
import { formatBytes, formatUnix } from "../lib/format";

export function Overview(props: { stats: StatsResponse; health?: "ok" | "degraded" | "fail"; roots: Node[]; error?: string; notice?: string }) {
  return (
    <Layout
      active="overview"
      title="Overview"
      description="Runtime health, storage totals, and the fastest system actions."
      mounts={props.stats.mounts.length}
      health={props.health}
      error={props.error}
      notice={props.notice}
    >
      <section class="summary" style="view-transition-name: fg-overview-summary">
        <div>
          <div class="label">Files</div>
          <div class="metric">{props.stats.index.totalFiles}</div>
        </div>
        <div>
          <div class="label">Directories</div>
          <div class="metric">{props.stats.index.totalDirs}</div>
        </div>
        <div>
          <div class="label">Entities</div>
          <div class="metric">{props.stats.index.totalEntities}</div>
        </div>
        <div>
          <div class="label">Path cache</div>
          <div class="metric">{Math.round(props.stats.cache.pathUtilRatio * 100)}%</div>
          <div class="label">
            {props.stats.cache.pathEntries} / {props.stats.cache.pathCapacity}
          </div>
        </div>
        <div>
          <div class="label">Generated</div>
          <div class="metric small-metric">{formatUnix(props.stats.generatedAt)}</div>
        </div>
      </section>
      <section class="grid">
        <div class="panel" style="view-transition-name: fg-overview-mounts">
          <div class="panel-head">
            <h2>Mounts</h2>
            <a href="/files">Open files</a>
          </div>
          <NodeTable
            nodes={props.roots}
            emptyTitle="No mounts configured"
            emptyText="Configure storage base paths to browse files here."
            viewTransitionName="fg-overview-mounts-table"
          />
        </div>
        <aside class="panel" style="view-transition-name: fg-overview-storage">
          <div class="panel-head">
            <h2>Storage</h2>
          </div>
          <div class="panel-body">
            <dl class="cfg">
              {props.stats.disks.map((disk) => (
                <div class="cfg-row">
                  <dt>{disk.diskName || disk.fsType}</dt>
                  <dd>
                    {formatBytes(disk.used)} / {formatBytes(disk.size)}
                  </dd>
                </div>
              ))}
            </dl>
          </div>
        </aside>
      </section>
    </Layout>
  );
}
