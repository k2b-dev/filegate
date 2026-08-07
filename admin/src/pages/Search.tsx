import type { GlobSearchResponse, StatsResponse } from "@k2b/filegate";
import { Layout } from "../components/Layout";
import { IconLabel } from "../components/Icons";
import { NodeTable } from "../components/Table";

export function Search(props: { stats: StatsResponse; health?: "ok" | "degraded" | "fail"; pattern: string; hidden: boolean; results?: GlobSearchResponse; error?: string }) {
  return (
    <Layout
      active="search"
      title="Search"
      description="Find indexed files and directories by glob pattern."
      mounts={props.stats.mounts.length}
      health={props.health}
      error={props.error}
    >
      <section class="panel" style="view-transition-name: fg-search-panel">
        <form class="toolbar search-toolbar" method="get" action="/search">
          <input class="input" name="pattern" value={props.pattern} placeholder="Glob pattern, e.g. **/*.jpg" aria-label="Glob pattern" />
          <select class="select" name="hidden">
            <option value="false" selected={!props.hidden}>
              Hidden off
            </option>
            <option value="true" selected={props.hidden}>
              Hidden on
            </option>
          </select>
          <button class="btn primary">
            <IconLabel icon="search">Search</IconLabel>
          </button>
        </form>
        {props.results?.errors.map((err) => (
          <div class="panel-body error-row">
            <div class="error">
              {err.path}: {err.cause}
            </div>
          </div>
        ))}
        <NodeTable
          nodes={props.results?.results ?? []}
          emptyTitle={props.pattern ? "No matches" : "Search the index"}
          emptyText={props.pattern ? "Nothing matched that pattern." : "Enter a glob pattern to find files and directories."}
          viewTransitionName="fg-search-results"
        />
      </section>
    </Layout>
  );
}
