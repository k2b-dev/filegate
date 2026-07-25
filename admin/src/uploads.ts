import {
  directUploads,
  upload,
  type BrowserUploadAllowResponse,
  type BrowserUploadConflictMode,
  type BrowserUploadEvent,
  type CapabilitiesResponse,
  type UploadSessionResponse,
} from "@valentinkolb/filegate";

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

async function runUpload(form: HTMLFormElement, files: File[]) {
  const panel = document.getElementById("fg-uploads");
  if (!panel) throw new Error("upload panel missing");
  const list = must(panel, ".uploads-list");
  const title = must(panel, ".uploads-title");
  const formData = new FormData(form);
  const parentPath = formData.get("parentPath")?.toString() ?? "";
  const onConflict = conflictModeFrom(formData.get("onConflict")?.toString());
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

document.addEventListener("click", (event) => {
  const target = event.target;
  if (!(target instanceof Element)) return;
  const trigger = target.closest<HTMLElement>("[data-upload-trigger]");
  if (!trigger) return;
  const input = document.getElementById(trigger.dataset.uploadTrigger === "folder" ? "admin-folder-upload" : "admin-file-upload");
  if (input instanceof HTMLInputElement) {
    input.value = "";
    input.click();
  }
});

document.addEventListener("change", (event) => {
  const input = event.target;
  if (!(input instanceof HTMLInputElement) || !input.matches("[data-upload-input]") || !input.files?.length || !input.form) return;
  const files = Array.from(input.files);
  const form = input.form;
  input.value = "";
  runUpload(form, files).catch((error) => {
    const panel = document.getElementById("fg-uploads");
    if (!panel) return;
    panel.hidden = false;
    must(panel, ".uploads-title").textContent = friendly(error);
  });
});
