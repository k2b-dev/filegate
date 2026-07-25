import type { Node, StatsResponse } from "@valentinkolb/filegate";
import { Layout } from "../components/Layout";
import { FileIcon, FolderIcon } from "../components/Icons";
import { NodeTable } from "../components/Table";
import { formatBytes, formatUnix } from "../lib/format";

type Crumb = { name: string; path?: string };

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
            <span class="count">
              {props.truncated ? "first " : ""}
              {props.children.length} item{props.children.length === 1 ? "" : "s"}
            </span>
          </div>
          {props.current?.type === "directory" && (
            <div class="toolbar">
              <button class="btn" type="button" data-mkdir-open data-mkdir-parent={props.current.path}>
                Create folder
              </button>
              <form class="tb-group tb-right" data-upload-form>
                <input type="hidden" name="parentPath" value={props.current.path} />
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
          <NodeTable
            nodes={props.children}
            selectedId={props.selected?.id}
            emptyTitle={props.current ? "This folder is empty" : "No mount roots"}
            emptyText={props.current ? "Create a folder to get started." : "Configure storage base paths to browse files here."}
            viewTransitionName="fg-files-table"
          />
        </div>
        <aside class="stack">
          {props.selected ? <Detail node={props.selected} /> : <EmptyDetail />}
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
              <button class="btn" type="button" data-transfer-open data-transfer-id={node.id} data-transfer-name={node.name} data-transfer-path={node.path}>
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
