import type { Node } from "@k2b/filegate";
import { FileIcon, FolderIcon, Icon } from "./Icons";
import { formatBytes, formatUnix } from "../lib/format";

export type SortField = "name" | "size" | "modified";
export type SortDirection = "asc" | "desc";
export type Sort = { field: SortField; direction: SortDirection };

const columns: { field: SortField; label: string; numeric?: boolean }[] = [
  { field: "name", label: "Name" },
  { field: "size", label: "Size", numeric: true },
  { field: "modified", label: "Modified" },
];

/**
 * Orders a listing.
 *
 * Directories stay ahead of files regardless of the sort field, matching the
 * server's listing order and how file managers behave: sorting by size should
 * not scatter folders through the files.
 */
export function sortNodes(nodes: Node[], sort: Sort): Node[] {
  const factor = sort.direction === "asc" ? 1 : -1;
  return [...nodes].sort((a, b) => {
    if (a.type !== b.type) return a.type === "directory" ? -1 : 1;
    switch (sort.field) {
      case "size":
        return (a.size - b.size) * factor;
      case "modified":
        return (a.mtime - b.mtime) * factor;
      default:
        return a.name.localeCompare(b.name) * factor;
    }
  });
}

/** Filters by a case-insensitive substring of the name. */
export function filterNodes(nodes: Node[], query: string): Node[] {
  const needle = query.trim().toLowerCase();
  if (!needle) return nodes;
  return nodes.filter((node) => node.name.toLowerCase().includes(needle));
}

function sortHref(base: string, field: SortField, current: Sort): string {
  const params = new URLSearchParams(base);
  // Clicking the active column flips direction; a different column starts
  // ascending, which is the least surprising default.
  const direction = current.field === field && current.direction === "asc" ? "desc" : "asc";
  params.set("sort", field);
  params.set("dir", direction);
  return `/files?${params}`;
}

function ariaSort(field: SortField, current: Sort): "ascending" | "descending" | "none" {
  if (current.field !== field) return "none";
  return current.direction === "asc" ? "ascending" : "descending";
}

export function NodeTable(props: {
  nodes: Node[];
  selectedId?: string;
  emptyTitle: string;
  emptyText: string;
  viewTransitionName?: string;
  sort?: Sort;
  /** Query string carrying the current path, so sort links keep the folder. */
  baseQuery?: string;
  /** Enables per-row checkboxes for bulk actions. */
  selectable?: boolean;
}) {
  const sort = props.sort ?? { field: "name" as SortField, direction: "asc" as SortDirection };
  const base = props.baseQuery ?? "";
  const columnCount = columns.length + 1 + (props.selectable ? 1 : 0);

  return (
    <div class="table-wrap" style={props.viewTransitionName ? `view-transition-name: ${props.viewTransitionName}` : undefined}>
      <table>
        <thead>
          <tr>
            {props.selectable && (
              <th class="pick">
                <input type="checkbox" data-bulk-all aria-label="Select all" />
              </th>
            )}
            {columns.map((column) => (
              <th class={column.numeric ? "num" : ""} aria-sort={ariaSort(column.field, sort)}>
                <a class="sort-link" href={sortHref(base, column.field, sort)}>
                  {column.label}
                  {sort.field === column.field && <Icon name={sort.direction === "asc" ? "chevron-up" : "chevron-down"} />}
                </a>
              </th>
            ))}
            <th>Type</th>
          </tr>
        </thead>
        <tbody>
          {props.nodes.map((node) => (
            <tr data-node-kind={node.type} class={props.selectedId === node.id ? "is-selected" : ""} style={nodeTransitionName(node.id)}>
              {props.selectable && (
                <td class="pick">
                  <input type="checkbox" data-bulk-item={node.id} data-bulk-name={node.name} aria-label={`Select ${node.name}`} />
                </td>
              )}
              <td>
                <span class="name-cell">
                  {node.type === "directory" ? <FolderIcon /> : <FileIcon />}
                  {node.type === "directory" ? (
                    <a data-node-open href={`/files?path=${encodeURIComponent(node.path)}`}>
                      {node.name}
                    </a>
                  ) : (
                    <a data-node-select href={`/files?path=${encodeURIComponent(parentPath(node.path))}&id=${encodeURIComponent(node.id)}`}>
                      {node.name}
                    </a>
                  )}
                </span>
              </td>
              <td class="num">{formatBytes(node.size)}</td>
              <td class="muted">{formatUnix(node.mtime)}</td>
              <td class="muted">{node.type === "directory" ? "Folder" : "File"}</td>
            </tr>
          ))}
          {props.nodes.length === 0 && (
            <tr class="empty-row">
              <td colspan={String(columnCount)}>
                <div class="empty">
                  <strong>{props.emptyTitle}</strong>
                  <span>{props.emptyText}</span>
                </div>
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  );
}

function parentPath(path: string): string {
  const clean = path.replace(/^\/+|\/+$/g, "");
  const idx = clean.lastIndexOf("/");
  return idx < 0 ? "" : clean.slice(0, idx);
}

function nodeTransitionName(id: string): string {
  return `view-transition-name: fg-node-${id.replace(/[^a-zA-Z0-9_-]/g, "-")}`;
}
