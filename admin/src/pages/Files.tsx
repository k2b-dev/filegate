import type { Node, StatsResponse, VersionResponse } from "@valentinkolb/filegate";
import { Layout } from "../components/Layout";
import { FileIcon, FolderIcon } from "../components/Icons";
import { NodeTable } from "../components/Table";
import { Versions } from "./Versions";
import { formatBytes, formatUnix } from "../lib/format";

type Crumb = { name: string; path?: string };

/** Thumbnails exist only for these; anything else keeps its file icon. */
const thumbnailTypes = new Set(["image/jpeg", "image/png", "image/gif", "image/webp"]);

function hasThumbnail(node: Node): boolean {
  return node.type === "file" && !!node.mimeType && thumbnailTypes.has(node.mimeType.split(";")[0]!.trim());
}

function viewHref(props: { current?: Node }, view: "list" | "grid"): string {
  const q = new URLSearchParams();
  if (props.current?.path) q.set("path", props.current.path.replace(/^\/+/, ""));
  if (view === "grid") q.set("view", "grid");
  const suffix = q.toString();
  return `/files${suffix ? `?${suffix}` : ""}`;
}

/**
 * Grid layout with image previews.
 *
 * The thumbnail sits on top of the icon rather than replacing it, so a file
 * whose thumbnail is unsupported, too large, or rejected because the job queue
 * is full simply keeps showing its icon. No error handling script needed.
 */
function NodeGrid(props: { nodes: Node[]; selectedId?: string; currentPath: string }) {
  if (props.nodes.length === 0) {
    return (
      <div class="panel-body">
        <p class="muted">This folder is empty.</p>
      </div>
    );
  }
  return (
    <ul class="grid-view">
      {props.nodes.map((node) => (
        <li class={`grid-item${node.id === props.selectedId ? " is-selected" : ""}`}>
          <a href={node.type === "directory" ? `/files?path=${encodeURIComponent(node.path.replace(/^\/+/, ""))}&view=grid` : `/files?path=${encodeURIComponent(props.currentPath.replace(/^\/+/, ""))}&id=${node.id}&view=grid`}>
            <span class="thumb">
              {node.type === "directory" ? <FolderIcon /> : <FileIcon />}
              {hasThumbnail(node) && (
                <img src={`/files/thumbnail?id=${node.id}&size=256`} alt="" loading="lazy" decoding="async" />
              )}
            </span>
            <span class="grid-name">{node.name}</span>
            <span class="grid-meta">{node.type === "directory" ? "Folder" : formatBytes(node.size)}</span>
          </a>
        </li>
      ))}
    </ul>
  );
}

/** Virtual path of the folder containing `path`, or "" for a mount root. */
function parentFolderOf(path: string): string {
  const clean = path.replace(/^\/+|\/+$/g, "");
  const cut = clean.lastIndexOf("/");
  return cut < 0 ? "" : clean.slice(0, cut);
}
const folderPickerAttrs = { webkitdirectory: "", directory: "" } as Record<string, string>;

export function Files(props: {
  stats: StatsResponse;
  health?: "ok" | "degraded" | "fail";
  crumbs: Crumb[];
  current?: Node;
  children: Node[];
  selected?: Node;
  error?: string;
  notice?: string;
  truncated?: boolean;
  versions?: { items?: VersionResponse[]; unsupported?: boolean };
  view?: "list" | "grid";
}) {
  return (
    <Layout
      active="files"
      title="Files"
      description="Browse mounts, manage files, and inspect node metadata."
      mounts={props.stats.mounts.length}
      health={props.health}
      error={props.error}
      notice={props.notice}
    >
      <section class="files-grid">
        <div class="panel" style="view-transition-name: fg-files-list">
          <div class="panel-head filelist-head">
            <nav class="crumbs" aria-label="Folder path">
              {props.crumbs.map((crumb, index) => (
                <>
                  {index > 0 && <span class="crumb-sep">/</span>}
                  <a href={crumb.path ? `/files?path=${encodeURIComponent(crumb.path)}` : "/files"}>{crumb.name}</a>
                </>
              ))}
            </nav>
            <span class="head-right">
              <span class="count">
                {props.truncated ? "first " : ""}
                {props.children.length} item{props.children.length === 1 ? "" : "s"}
              </span>
              <span class="view-toggle" role="group" aria-label="Layout">
                <a class={props.view === "grid" ? "" : "active"} href={viewHref(props, "list")} aria-current={props.view === "grid" ? undefined : "true"}>
                  List
                </a>
                <a class={props.view === "grid" ? "active" : ""} href={viewHref(props, "grid")} aria-current={props.view === "grid" ? "true" : undefined}>
                  Grid
                </a>
              </span>
            </span>
          </div>
          {props.current?.type === "directory" && (
            <div class="toolbar">
              <button class="btn" type="button" data-mkdir-open data-mkdir-parent={props.current.path}>
                Create folder
              </button>
              <form class="tb-group tb-right" data-upload-form>
                <input type="hidden" name="parentPath" value={props.current.path} />
                <label class="tb-label" for="upload-conflict">
                  If it exists
                </label>
                <select id="upload-conflict" class="select" name="onConflict">
                  <option value="skip-existing">Skip</option>
                  <option value="skip-identical">Skip if identical</option>
                  <option value="rename">Keep both</option>
                  <option value="overwrite">Overwrite</option>
                  <option value="error">Fail</option>
                </select>
                <input id="admin-file-upload" type="file" multiple data-upload-input hidden />
                <input id="admin-folder-upload" type="file" multiple data-upload-input hidden {...folderPickerAttrs} />
                <button class="btn primary" type="button" data-upload-trigger="file">
                  Upload file
                </button>
                <button class="btn" type="button" data-upload-trigger="folder">
                  Upload folder
                </button>
              </form>
            </div>
          )}
          {props.view === "grid" ? (
            <NodeGrid nodes={props.children} selectedId={props.selected?.id} currentPath={props.current?.path ?? ""} />
          ) : (
            <NodeTable
              nodes={props.children}
              selectedId={props.selected?.id}
              emptyTitle={props.current ? "This folder is empty" : "No mount roots"}
              emptyText={props.current ? "Create a folder to get started." : "Configure storage base paths to browse files here."}
              viewTransitionName="fg-files-table"
            />
          )}
        </div>
        <aside class="stack">
          {props.selected ? <Detail node={props.selected} /> : <EmptyDetail />}
          {props.selected?.type === "file" && props.versions && (
            <Versions
              fileId={props.selected.id}
              parentPath={parentFolderOf(props.selected.path)}
              versions={props.versions.items}
              unsupported={props.versions.unsupported}
            />
          )}
        </aside>
      </section>
      <UploadPanel />
      <script src="/uploads.js" defer />
    </Layout>
  );
}

function UploadPanel() {
  return (
    <div id="fg-uploads" class="uploads" hidden>
      <div class="uploads-head">
        <span class="uploads-title">Uploading...</span>
        <button type="button" class="btn uploads-cancel">Cancel</button>
        <button type="button" class="uploads-close" aria-label="Close">
          ×
        </button>
      </div>
      <div class="uploads-list" />
      <div class="uploads-stats" aria-label="Upload statistics">
        <div>
          <span>Throughput</span>
          <strong data-upload-rate>—</strong>
        </div>
        <div>
          <span>Remaining</span>
          <strong data-upload-eta>—</strong>
        </div>
        <div>
          <span>Elapsed</span>
          <strong data-upload-elapsed>0s</strong>
        </div>
        <div>
          <span>Transferred</span>
          <strong data-upload-bytes>0 B</strong>
        </div>
      </div>
    </div>
  );
}

function Detail(props: { node: Node }) {
  const node = props.node;
  return (
    <section class="panel detail" style="view-transition-name: fg-files-detail">
      <div class="panel-head detail-head">
        <span class="name-cell">
          {node.type === "directory" ? <FolderIcon /> : <FileIcon />}
          <h2 title={node.name}>{node.name}</h2>
        </span>
        <span class="tag">{node.type === "directory" ? "Folder" : "File"}</span>
      </div>
      <div class="panel-body">
        <div class="detail-section">
          <h3 class="section-title">Properties</h3>
          <dl class="props detail-props">
            <div class="prop">
              <dt>Size</dt>
              <dd>{formatBytes(node.size)}</dd>
            </div>
            <div class="prop">
              <dt>Modified</dt>
              <dd>{formatUnix(node.mtime)}</dd>
            </div>
            {node.type === "file" && (
              <div class="prop">
                <dt>MIME type</dt>
                <dd class="mono">{node.mimeType || "-"}</dd>
              </div>
            )}
            <div class="prop">
              <dt>Owner</dt>
              <dd>
                {node.ownership.uid}:{node.ownership.gid}
              </dd>
            </div>
            <div class="prop">
              <dt>Mode</dt>
              <dd>{node.ownership.mode}</dd>
            </div>
            <div class="prop">
              <dt>Path</dt>
              <dd class="mono">{node.path}</dd>
            </div>
            <div class="prop">
              <dt>Node ID</dt>
              <dd class="mono">{node.id}</dd>
            </div>
          </dl>
        </div>
        <div class="detail-section">
          <h3 class="section-title">Actions</h3>
          <div class="action-list">
            <div class="action-row">
              <div>
                <strong>Download</strong>
                <span class="muted">{node.type === "directory" ? "Download as archive." : "Download directly."}</span>
              </div>
              <a class="btn" href={`/files/download?id=${encodeURIComponent(node.id)}`}>
                Download
              </a>
            </div>
            <div class="action-row">
              <div>
                <strong>Rename</strong>
                <span class="muted">Change the name in this folder.</span>
              </div>
              <button class="btn" type="button" data-rename-open data-rename-id={node.id} data-rename-name={node.name} data-rename-path={node.path}>
                Rename
              </button>
            </div>
            <div class="action-row">
              <div>
                <strong>Move or copy</strong>
                <span class="muted">Transfer to another folder.</span>
              </div>
              <button class="btn" type="button" data-transfer-open data-transfer-id={node.id} data-transfer-name={node.name} data-transfer-path={node.path} data-transfer-parent={parentFolderOf(node.path)}>
                Transfer
              </button>
            </div>
            <div class="action-row">
              <div>
                <strong>Metadata</strong>
                <span class="muted">Edit owner, group, and mode.</span>
              </div>
              <button
                class="btn"
                type="button"
                data-metadata-open
                data-metadata-id={node.id}
                data-metadata-path={node.path}
                data-metadata-kind={node.type}
                data-metadata-uid={String(node.ownership.uid)}
                data-metadata-gid={String(node.ownership.gid)}
                data-metadata-mode={node.ownership.mode}
              >
                Edit metadata
              </button>
            </div>
          </div>
        </div>
        <div class="detail-section">
          <h3 class="section-title">Danger zone</h3>
          <div class="action-list">
            <div class="action-row action-row-danger">
              <div>
                <strong>Delete</strong>
                <span class="muted">Remove this resource permanently.</span>
              </div>
              <form method="post" action="/files/delete" data-confirm-delete={node.path}>
                <input type="hidden" name="id" value={node.id} />
                {/* Lets a failed delete return the user to this folder. */}
                <input type="hidden" name="parentPath" value={parentFolderOf(node.path)} />
                <button class="btn danger">Delete</button>
              </form>
            </div>
          </div>
        </div>
      </div>
    </section>
  );
}

function EmptyDetail() {
  return (
    <section class="panel" style="view-transition-name: fg-files-detail">
      <div class="panel-body empty detail-empty">
        <strong>No resource selected</strong>
        <span>Open a folder, then select a file or folder to inspect its properties and manage it.</span>
      </div>
    </section>
  );
}
