import { showFileDialog, showFolderDialog } from "@valentinkolb/stdlib/browser";
import {
  directUploads,
  upload,
  type BrowserUploadAllowResponse,
  type BrowserUploadConflictMode,
  type BrowserUploadFile,
  type BrowserUploadEvent,
  type CapabilitiesResponse,
  type UploadSessionResponse,
} from "@k2b/filegate";

const FALLBACK_SEGMENT_SIZE = 8 * 1024 * 1024;
const PREFERRED_SEGMENT_SIZE = 32 * 1024 * 1024;
const PREFERRED_DIRECT_THRESHOLD = 50 * 1024 * 1024;

type UploadRow = {
  set(pct: number, text?: string, cls?: string): void;
};

type UploadStats = {
  set(event: Extract<BrowserUploadEvent, { type: "stats" }>): void;
  stop(): void;
};

function friendly(error: unknown): string {
  if (error instanceof Error) return error.message.replace(/^filegate api error \(\d+\): /, "");
  if (typeof error === "string") return error;
  return "Upload failed";
}

async function postJSON<T>(url: string, body: unknown): Promise<T> {
  const res = await fetch(url, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  let data: { error?: string } = {};
  try {
    data = (await res.json()) as { error?: string };
  } catch {
    // Empty success/error bodies are rendered from status below.
  }
  if (!res.ok) throw new Error(data.error || `Upload failed (${res.status})`);
  return data as T;
}

async function getJSON<T>(url: string): Promise<T> {
  const res = await fetch(url, { credentials: "same-origin" });
  if (!res.ok) throw new Error(`Request failed (${res.status})`);
  return (await res.json()) as T;
}

async function uploadConfig(): Promise<{ segmentSize: number; directThresholdBytes: number }> {
  try {
    const caps = await getJSON<CapabilitiesResponse>("/api/capabilities");
    const maxChunk = caps.uploads.maxChunkBytes;
    const maxUpload = caps.uploads.maxUploadBytes;
    const segmentSize = maxChunk > 0 ? Math.min(PREFERRED_SEGMENT_SIZE, maxChunk) : FALLBACK_SEGMENT_SIZE;
    const directLimit = Math.min(
      PREFERRED_DIRECT_THRESHOLD,
      maxChunk > 0 ? maxChunk : PREFERRED_DIRECT_THRESHOLD,
      maxUpload > 0 ? maxUpload : PREFERRED_DIRECT_THRESHOLD,
    );
    return { segmentSize, directThresholdBytes: directLimit };
  } catch {
    return { segmentSize: FALLBACK_SEGMENT_SIZE, directThresholdBytes: FALLBACK_SEGMENT_SIZE };
  }
}

function fmtBytes(n: number) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  let v = n || 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${i ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

function fmtDuration(sec: number) {
  if (!Number.isFinite(sec) || sec < 0) return "-";
  const rounded = Math.round(sec);
  if (rounded < 60) return `${rounded}s`;
  const min = Math.floor(rounded / 60);
  const s = rounded % 60;
  if (min < 60) return `${min}m ${s}s`;
  return `${Math.floor(min / 60)}h ${min % 60}m`;
}

function bindStats(panel: HTMLElement): UploadStats {
  const rateEl = must(panel, "[data-upload-rate]");
  const etaEl = must(panel, "[data-upload-eta]");
  const elapsedEl = must(panel, "[data-upload-elapsed]");
  const bytesEl = must(panel, "[data-upload-bytes]");
  let last: Extract<BrowserUploadEvent, { type: "stats" }> | undefined;

  function paint() {
    if (!last) return;
    rateEl.textContent = last.transferred ? `${fmtBytes(last.bytesPerSecond)}/s` : "-";
    etaEl.textContent = last.remainingMs === undefined ? "-" : fmtDuration(last.remainingMs / 1000);
    elapsedEl.textContent = fmtDuration(last.elapsedMs / 1000);
    bytesEl.textContent = `${fmtBytes(last.transferred)} / ${fmtBytes(last.total)}`;
  }

  const timer = window.setInterval(paint, 1000);
  return {
    set(event) {
      last = event;
      paint();
    },
    stop() {
      window.clearInterval(timer);
      paint();
    },
  };
}

type DirectGrant = NonNullable<UploadSessionResponse["direct"]>;

const conflictModes: BrowserUploadConflictMode[] = ["skip-existing", "skip-identical", "overwrite", "rename", "error"];

function conflictModeFrom(raw: string | undefined): BrowserUploadConflictMode {
  const found = conflictModes.find((mode) => mode === raw);
  // skip-existing stays the default: it is the only mode that cannot destroy
  // data when someone re-drops a folder they already uploaded.
  return found ?? "skip-existing";
}

/** Best-effort abort of every session created for a cancelled upload. */
async function abortGrants(grants: { direct: DirectGrant }[]): Promise<void> {
  await Promise.allSettled(grants.map((grant) => directUploads.abort({ direct: grant.direct })));
}

function makeRow(list: HTMLElement, name: string): UploadRow {
  const el = document.createElement("div");
  el.className = "up-item";
  el.innerHTML = '<div class="up-row"><span class="up-name"></span><span class="up-stat">Queued</span></div><div class="up-bar"><div class="up-fill"></div></div>';
  must(el, ".up-name").textContent = name;
  list.appendChild(el);
  return {
    set(pct, text, cls) {
      (must(el, ".up-fill") as HTMLElement).style.width = `${Math.max(0, Math.min(100, pct || 0))}%`;
      if (text) must(el, ".up-stat").textContent = text;
      el.className = `up-item${cls ? ` ${cls}` : ""}`;
    },
  };
}

async function runUpload(parentPath: string, files: StagedFile[], onConflict: BrowserUploadConflictMode) {
  const panel = document.getElementById("fg-uploads");
  if (!panel) throw new Error("upload panel missing");
  const list = must(panel, ".uploads-list");
  const title = must(panel, ".uploads-title");
  const reloadURL = `/files${parentPath ? `?path=${encodeURIComponent(parentPath)}` : ""}`;
  const rows = new Map<string, UploadRow>();
  let completed = 0;
  let failed = 0;
  let skipped = 0;

  list.innerHTML = "";
  panel.hidden = false;
  must(panel, ".uploads-close").addEventListener("click", () => location.assign(reloadURL), { once: true });

  // Cancelling stops the client immediately and aborts the sessions the server
  // already created, so an interrupted upload does not leave orphans holding
  // staged bytes until the cleanup loop notices.
  const controller = new AbortController();
  const grants: { direct: DirectGrant }[] = [];
  const cancelButton = panel.querySelector<HTMLButtonElement>(".uploads-cancel");
  let cancelled = false;
  const cancel = () => {
    if (cancelled) return;
    cancelled = true;
    controller.abort(new Error("Upload cancelled"));
    if (cancelButton) cancelButton.disabled = true;
    title.textContent = "Cancelling...";
    void abortGrants(grants);
  };
  cancelButton?.addEventListener("click", cancel);
  files.forEach((file, index) => rows.set(`u${index + 1}`, makeRow(list, file.webkitRelativePath || file.name)));

  const stats = bindStats(panel);
  const setTitle = () => {
    title.textContent =
      files.length > 1
        ? `Uploading ${completed} of ${files.length} files${skipped ? ` - ${skipped} skipped` : ""}${failed ? ` - ${failed} failed` : ""}`
        : failed
          ? "Upload failed"
          : skipped
            ? "Upload skipped"
          : completed
            ? "Upload complete"
            : "Uploading file";
  };

  title.textContent = files.length > 1 ? `Preparing ${files.length} files` : "Preparing upload";
  const cfg = await uploadConfig();
  const result = await upload({
    files,
    path: parentPath,
    signal: controller.signal,
    allow: async (req) => {
      const res = await postJSON<BrowserUploadAllowResponse>("/api/uploads/sessions", req);
      for (const entry of res.uploads) {
        const direct = entry.ok ? (entry.upload?.kind === "session" ? entry.upload.session.direct : undefined) : undefined;
        if (direct) grants.push({ direct });
      }
      return res;
    },
    config: {
      segmentSize: cfg.segmentSize,
      directThresholdBytes: cfg.directThresholdBytes,
      onConflict,
      chunkSize: 4 * 1024 * 1024,
      concurrency: { hash: 2, files: 6, segments: 6 },
      batch: { size: 32, flushMs: 20 },
    },
    onEvent(event) {
      if (event.type === "stats") {
        stats.set(event);
        return;
      }
      const row = "id" in event ? rows.get(event.id) : undefined;
      if (event.type === "file:hashing") row?.set(event.total ? Math.max(2, Math.round((event.loaded / event.total) * 14)) : 2, "Hashing...");
      if (event.type === "file:allowed") row?.set(15, "Queued");
      if (event.type === "file:uploading") row?.set(15 + Math.round((event.loaded / event.total) * 80), "Uploading...");
      if (event.type === "file:committing") row?.set(97, "Committing...");
      if (event.type === "file:done") {
        completed++;
        row?.set(100, "Done", "done");
        setTitle();
      }
      if (event.type === "file:skipped") {
        completed++;
        skipped++;
        row?.set(100, event.reason || "Skipped", "skip");
        setTitle();
      }
      if (event.type === "file:rejected") {
        completed++;
        failed++;
        row?.set(100, event.error, "err");
        setTitle();
      }
      if (event.type === "file:error") {
        completed++;
        failed++;
        row?.set(100, friendly(event.error), "err");
        setTitle();
      }
    },
  });

  stats.stop();
  if (result.failed === 0 && result.rejected === 0) {
    title.textContent = result.skipped ? `Upload complete - ${result.skipped} skipped` : "Upload complete";
    window.setTimeout(() => location.assign(reloadURL), 700);
  } else {
    title.textContent = `${result.failed + result.rejected} of ${files.length} failed`;
  }
}

function must(root: ParentNode, selector: string): HTMLElement {
  const el = root.querySelector(selector);
  if (!(el instanceof HTMLElement)) throw new Error(`missing ${selector}`);
  return el;
}

/**
 * Files staged for upload.
 *
 * Not necessarily real File objects: entries dropped as part of a folder are
 * wrapped so they can carry a relative path, which a File from the drag-and-drop
 * entry API does not have. The SDK's upload contract is structural, so a wrapper
 * with name, size and slice is all it needs.
 */
type StagedFile = BrowserUploadFile;

function stagedFrom(file: File, relativePath?: string): StagedFile {
  if (!relativePath || relativePath === file.name) return file;
  return {
    name: file.name,
    size: file.size,
    type: file.type,
    webkitRelativePath: relativePath,
    slice: (start, end) => file.slice(start, end),
  };
}

/** Reads a dropped directory tree, preserving relative paths. */
async function readEntry(entry: FileSystemEntry, prefix: string, out: StagedFile[]): Promise<void> {
  if (entry.isFile) {
    const file = await new Promise<File>((resolve, reject) => (entry as FileSystemFileEntry).file(resolve, reject));
    out.push(stagedFrom(file, prefix ? `${prefix}/${file.name}` : file.name));
    return;
  }
  if (!entry.isDirectory) return;

  const reader = (entry as FileSystemDirectoryEntry).createReader();
  const next = prefix ? `${prefix}/${entry.name}` : entry.name;
  // readEntries returns at most 100 entries per call, so keep reading until
  // it reports an empty batch or a large folder silently loses its tail.
  for (;;) {
    const batch = await new Promise<FileSystemEntry[]>((resolve, reject) => reader.readEntries(resolve, reject));
    if (batch.length === 0) return;
    for (const child of batch) await readEntry(child, next, out);
  }
}

async function stagedFromDrop(transfer: DataTransfer): Promise<StagedFile[]> {
  const entries = Array.from(transfer.items)
    .map((item) => (typeof item.webkitGetAsEntry === "function" ? item.webkitGetAsEntry() : null))
    .filter((entry): entry is FileSystemEntry => !!entry);

  if (entries.length === 0) return Array.from(transfer.files);

  const out: StagedFile[] = [];
  for (const entry of entries) await readEntry(entry, "", out);
  return out;
}

function label(file: StagedFile): string {
  return file.webkitRelativePath || file.name;
}

function openUploadDialog(parentPath: string): void {
  const staged = new Map<string, StagedFile>();

  const dialog = document.createElement("dialog");
  dialog.className = "prompt";
  dialog.innerHTML = `
    <div class="prompt-panel">
      <div class="prompt-head">
        <h2>Upload</h2>
        <button type="button" class="prompt-close" data-close aria-label="Close dialog">&times;</button>
      </div>
      <div class="prompt-body">
        <div class="prompt-message"><span class="prompt-badge"></span></div>
        <button type="button" class="dropzone" data-dropzone>
          <i class="ti ti-cloud-upload dropzone-icon" aria-hidden="true"></i>
          <strong>Drop files or folders here</strong>
          <span class="muted">or click to choose files</span>
        </button>
        <ul class="staged" data-staged hidden></ul>
        <div class="field">
          <label for="upload-conflict">If a file already exists</label>
          <select id="upload-conflict" class="select" data-conflict>
            <option value="skip-existing">Skip it</option>
            <option value="skip-identical">Skip if identical</option>
            <option value="rename">Keep both</option>
            <option value="overwrite">Overwrite it</option>
            <option value="error">Fail the upload</option>
          </select>
        </div>
      </div>
      <div class="prompt-footer upload-footer">
        <button type="button" class="btn" data-folder><i class="ti ti-folder-up" aria-hidden="true"></i><span class="btn-label">Upload folder</span></button>
        <button type="button" class="btn primary" data-start disabled><i class="ti ti-upload" aria-hidden="true"></i><span class="btn-label" data-start-label>Upload</span></button>
      </div>
    </div>`;
  document.body.appendChild(dialog);

  const badge = must(dialog, ".prompt-badge");
  const zone = must(dialog, "[data-dropzone]");
  const list = must(dialog, "[data-staged]");
  const start = must(dialog, "[data-start]") as HTMLButtonElement;
  const conflict = must(dialog, "[data-conflict]") as HTMLSelectElement;

  badge.textContent = parentPath ? `/${parentPath}` : "mount root";

  const add = (incoming: StagedFile[]) => {
    for (const file of incoming) staged.set(`${label(file)}:${file.size}`, file);
    render();
  };

  const render = () => {
    const items = [...staged.values()];
    start.disabled = items.length === 0;
    const startLabel = start.querySelector("[data-start-label]");
    if (startLabel) {
      startLabel.textContent = items.length ? `Upload ${items.length} file${items.length === 1 ? "" : "s"}` : "Upload";
    }
    list.hidden = items.length === 0;
    list.innerHTML = "";
    for (const file of items) {
      const row = document.createElement("li");
      row.innerHTML = `<span class="staged-name"></span><button type="button" class="staged-remove" aria-label="Remove">&times;</button>`;
      must(row, ".staged-name").textContent = label(file);
      must(row, ".staged-remove").addEventListener("click", () => {
        staged.delete(`${label(file)}:${file.size}`);
        render();
      });
      list.appendChild(row);
    }
  };

  const close = () => {
    dialog.close();
    dialog.remove();
    document.documentElement.classList.remove("has-prompt");
  };

  zone.addEventListener("click", () => {
    void showFileDialog({ multiple: true })
      .then((picked) => add(Array.isArray(picked) ? picked : [picked]))
      // Cancelling the native dialog rejects; that is not an error worth showing.
      .catch(() => {});
  });

  for (const type of ["dragenter", "dragover"]) {
    zone.addEventListener(type, (event) => {
      event.preventDefault();
      zone.classList.add("is-over");
    });
  }
  for (const type of ["dragleave", "drop"]) {
    zone.addEventListener(type, () => zone.classList.remove("is-over"));
  }
  zone.addEventListener("drop", (event) => {
    event.preventDefault();
    const transfer = (event as DragEvent).dataTransfer;
    if (transfer) void stagedFromDrop(transfer).then(add);
  });

  must(dialog, "[data-folder]").addEventListener("click", () => {
    void showFolderDialog()
      .then(add)
      .catch(() => {});
  });

  must(dialog, "[data-close]").addEventListener("click", close);
  dialog.addEventListener("cancel", (event) => {
    event.preventDefault();
    close();
  });

  start.addEventListener("click", () => {
    const files = [...staged.values()];
    if (!files.length) return;
    const mode = conflictModeFrom(conflict.value);
    close();
    runUpload(parentPath, files, mode).catch((error) => {
      const panel = document.getElementById("fg-uploads");
      if (!panel) return;
      panel.hidden = false;
      must(panel, ".uploads-title").textContent = friendly(error);
    });
  });

  document.documentElement.classList.add("has-prompt");
  dialog.showModal();
}

document.addEventListener("click", (event) => {
  const target = event.target;
  if (!(target instanceof Element)) return;
  const trigger = target.closest<HTMLElement>("[data-upload-open]");
  if (!trigger) return;
  openUploadDialog(trigger.dataset.uploadOpen || "");
});
