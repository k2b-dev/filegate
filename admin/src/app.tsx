import {
  FilegateError,
  type ActivityListResponse,
  type BrowserUploadAllowItem,
  type BrowserUploadAllowRequest,
  type BrowserUploadAllowResult,
  type BrowserUploadConflictMode,
  type CapabilitiesResponse,
  type GlobSearchResponse,
  type VersionResponse,
  type HealthStatus,
  type DirectUploadURLResponse,
  type FileConflictMode,
  type MkdirConflictMode,
  type Node,
  type NodeListResponse,
  type Ownership,
  type StatsResponse,
  type UploadSessionCreateRequest,
  type UploadSessionDirectRequest,
} from "@valentinkolb/filegate";
import { Hono, type Context } from "hono";
import { withActor } from "./lib/actor";
import { authMethods, login, logout, oidcBegin, oidcCallback, requireAuth } from "./lib/auth";
import { client, isList, parentPath, resolveDirectory } from "./lib/filegate";
import { env } from "./lib/env";
import { errorMessage, formatRetryAfter, redirectFiles, selectedFiles } from "./lib/format";
import { config, routes, ssr } from "./config";
import { LoginPage } from "./components/Layout";
import { readThemeFromCookieHeader, type AdminTheme } from "./lib/theme";
import { Files } from "./pages/Files";
import { Overview } from "./pages/Overview";
import { Search } from "./pages/Search";
import { System } from "./pages/System";

type Crumb = { name: string; path?: string };
type ActivityQuery = { q: string; operation: string; outcome: string; page: number; pageSize: number };

const emptyStats: StatsResponse = {
  generatedAt: 0,
  index: { totalEntities: 0, totalFiles: 0, totalDirs: 0, dbSizeBytes: 0 },
  cache: { pathEntries: 0, pathCapacity: 0, pathUtilRatio: 0 },
  mounts: [],
  disks: [],
  system: { goroutines: 0, heapAllocBytes: 0, heapSysBytes: 0, heapObjects: 0, numGC: 0, lastGCPauseNs: 0, openFDs: 0, maxFDs: 0 },
};

export const app = new Hono()
  .route("/_ssr", routes(config))
  .get("/styles.css", (c) => {
    c.header("Content-Type", "text/css; charset=utf-8");
    return new Response(Bun.file(new URL("./styles.css", import.meta.url)));
  })
  .get("/uploads.js", (c) => {
    c.header("Content-Type", "text/javascript; charset=utf-8");
    return new Response(Bun.file(new URL("./uploads.js", import.meta.url)));
  })
  .get("/prompts.js", (c) => {
    c.header("Content-Type", "text/javascript; charset=utf-8");
    return new Response(Bun.file(new URL("./prompts.js", import.meta.url)));
  })
  .get("/theme.js", (c) => {
    c.header("Content-Type", "text/javascript; charset=utf-8");
    return new Response(Bun.file(new URL("./theme.js", import.meta.url)));
  })
  .get("/health", (c) => c.text("OK"))
  .get(
    "/login",
    ...ssr(async (c) => {
      setPage(c, "Sign in");
      const error = loginError(c.req.query("error"), c.req.query("retry"), c.req.query("reason"));
      const methods = authMethods();
      return () => <LoginPage error={error} methods={methods} />;
    }),
  )
  .post("/login", login)
  .get("/auth/login", oidcBegin)
  .get("/auth/callback", oidcCallback)
  .use("*", requireAuth())
  .use("*", withActor())
  .post("/logout", logout)
  .get(
    "/",
    ...ssr(async (c) => {
      setPage(c, "Overview");
      const data = await loadBase();
      return () => <Overview stats={data.stats} health={data.health} roots={data.roots} error={queryError(c.req.query("error"), data.error)} />;
    }),
  )
  .get(
    "/files",
    ...ssr(async (c) => {
      setPage(c, "Files");
      const data = await loadFiles(c.req.query("path") || "", c.req.query("id") || "");
      const loadError = "error" in data ? data.error : undefined;
      return () => <Files {...data} error={queryError(c.req.query("error"), loadError)} notice={c.req.query("notice")} />;
    }),
  )
  .post("/files/mkdir", async (c) => {
    const body = await c.req.parseBody();
    const path = field(body, "parentPath");
    try {
      const parent = await resolveDirectory(path);
      await client().nodes.mkdir(parent.id, { path: field(body, "name"), recursive: true, onConflict: mkdirConflictMode(field(body, "onConflict")) });
      return c.redirect(redirectFiles(path), 303);
    } catch (err) {
      return c.redirect(redirectFiles(path, errorMessage(err)), 303);
    }
  })
  .post("/api/uploads/sessions", async (c) => {
    try {
      const body = await c.req.json();
      const parsed = parseUploadAllowRequest(body);
      const out = await createUploadSessions(parsed.uploads, parsed.onConflict);
      return c.json({ uploads: out }, 201);
    } catch (err) {
      return c.json({ error: errorMessage(err) }, 400);
    }
  })
  .get("/api/capabilities", async (c) => {
    try {
      const out: CapabilitiesResponse = await client().capabilities.get();
      return c.json(out);
    } catch (err) {
      return c.json({ error: errorMessage(err) }, 400);
    }
  })
  .post("/files/delete", async (c) => {
    const body = await c.req.parseBody();
    try {
      const node = await client().nodes.get(field(body, "id"));
      await client().nodes.delete(node.id);
      return c.redirect(redirectFiles(parentPath(node.path)), 303);
    } catch (err) {
      return c.redirect(redirectFiles(field(body, "parentPath"), errorMessage(err)), 303);
    }
  })
  .get("/files/download", async (c) => {
    try {
      const nodeId = c.req.query("id")?.trim();
      if (!nodeId) throw new Error("node id required");
      const out = await client().downloads.createDirectURL({
        nodeId,
        expiresInSeconds: 300,
      });
      return c.redirect(out.downloadUrl, 303);
    } catch (err) {
      return c.redirect(redirectFiles(c.req.query("parentPath") || "", errorMessage(err)), 303);
    }
  })
  .post("/files/rename", async (c) => {
    const body = await c.req.parseBody();
    try {
      const updated = await client().nodes.patch(field(body, "id"), { name: field(body, "name") });
      return c.redirect(selectedFiles(parentPath(updated.path), updated.id), 303);
    } catch (err) {
      return c.redirect(selectedFiles(field(body, "parentPath"), field(body, "id"), errorMessage(err)), 303);
    }
  })
  .post("/files/metadata", async (c) => {
    const body = await c.req.parseBody();
    try {
      const updated = await client().nodes.patch(field(body, "id"), { ownership: ownershipFromForm(body) }, field(body, "recursiveOwnership") === "true");
      return c.redirect(selectedFiles(parentPath(updated.path), updated.id), 303);
    } catch (err) {
      return c.redirect(selectedFiles(field(body, "parentPath"), field(body, "id"), errorMessage(err)), 303);
    }
  })
  .post("/files/versions/snapshot", async (c) => {
    const body = await c.req.parseBody();
    return versionAction(c, body, () => client().versions.snapshot(field(body, "id"), field(body, "label") || undefined));
  })
  .post("/files/versions/pin", async (c) => {
    const body = await c.req.parseBody();
    return versionAction(c, body, () => client().versions.pin(field(body, "id"), field(body, "versionId"), field(body, "label") || undefined));
  })
  .post("/files/versions/unpin", async (c) => {
    const body = await c.req.parseBody();
    return versionAction(c, body, () => client().versions.unpin(field(body, "id"), field(body, "versionId")));
  })
  .post("/files/versions/delete", async (c) => {
    const body = await c.req.parseBody();
    return versionAction(c, body, () => client().versions.delete(field(body, "id"), field(body, "versionId")));
  })
  .post("/files/versions/restore", async (c) => {
    const body = await c.req.parseBody();
    const asNewFile = field(body, "asNewFile") === "true";
    try {
      const out = await client().versions.restore(field(body, "id"), field(body, "versionId"), {
        asNewFile,
        name: field(body, "name") || undefined,
      });
      // An as-new restore produces a different node, so select that one.
      return c.redirect(selectedFiles(parentPath(out.node.path), out.node.id, undefined, restoreNotice(out.asNew)), 303);
    } catch (err) {
      return c.redirect(selectedFiles(field(body, "parentPath"), field(body, "id"), errorMessage(err)), 303);
    }
  })
  .get("/files/versions/download", async (c) => {
    const id = c.req.query("id")?.trim();
    const versionId = c.req.query("versionId")?.trim();
    if (!id || !versionId) return c.redirect(redirectFiles("", "file and version are required"), 303);
    try {
      // Version bytes have no signed direct-URL endpoint, so unlike normal
      // downloads this one streams through the admin server.
      const upstream = await client().versions.contentRaw(id, versionId);
      return new Response(upstream.body, {
        status: upstream.status,
        headers: {
          "Content-Type": upstream.headers.get("content-type") ?? "application/octet-stream",
          "Content-Disposition": upstream.headers.get("content-disposition") ?? `attachment; filename="${versionId}.bin"`,
        },
      });
    } catch (err) {
      return c.redirect(redirectFiles(c.req.query("parentPath") || "", errorMessage(err)), 303);
    }
  })
  .post("/files/transfer", async (c) => {
    const body = await c.req.parseBody();
    try {
      const targetParent = await resolveDirectory(field(body, "targetParentPath"));
      const out = await client().transfers.create({
        op: field(body, "op") === "copy" ? "copy" : "move",
        sourceId: field(body, "id"),
        targetParentId: targetParent.id,
        targetName: field(body, "targetName"),
        onConflict: conflictMode(field(body, "onConflict")),
      });
      return c.redirect(selectedFiles(parentPath(out.node.path), out.node.id), 303);
    } catch (err) {
      return c.redirect(selectedFiles(field(body, "parentPath"), field(body, "id"), errorMessage(err)), 303);
    }
  })
  .get(
    "/search",
    ...ssr(async (c) => {
      setPage(c, "Search");
      const base = await loadBase();
      const pattern = c.req.query("pattern") || "";
      const hidden = c.req.query("hidden") === "true";
      let results: GlobSearchResponse | undefined;
      let error = base.error;
      try {
        results = pattern
          ? await client().search.glob({ pattern, limit: 100, showHidden: hidden, files: true, directories: true })
          : undefined;
      } catch (err) {
        // A bad glob pattern is user error, not an outage; either way it must
        // not escape as a raw 500.
        error = errorMessage(err);
      }
      return () => <Search stats={base.stats} health={base.health} pattern={pattern} hidden={hidden} results={results} error={error} />;
    }),
  )
  .get(
    "/system",
    ...ssr(async (c) => {
      setPage(c, "System");
      const activityQuery = parseActivityQuery({
        q: c.req.query("q"),
        operation: c.req.query("operation"),
        outcome: c.req.query("outcome"),
        page: c.req.query("page"),
      });
      const [base, activity] = await Promise.all([loadBase(), loadActivity(activityQuery)]);
      return () => (
        <System
          stats={base.stats}
          health={base.health}
          activity={activity}
          activityQuery={activityQuery}
          error={queryError(c.req.query("error"), base.error)}
          notice={c.req.query("notice")}
        />
      );
    }),
  )
  .post("/system/rescan", async (c) => {
    try {
      await client().index.rescan();
      return c.redirect("/system?notice=" + encodeURIComponent("Index rescan started"), 303);
    } catch (err) {
      return c.redirect("/system?error=" + encodeURIComponent(errorMessage(err)), 303);
    }
  });

const oidcErrors: Record<string, string> = {
  state: "The sign-in attempt expired or was started elsewhere. Please try again.",
  nonce: "The sign-in response did not match this attempt. Please try again.",
  group: "Your account is not a member of a group allowed to use this admin.",
  provider: "The identity provider declined the sign-in.",
  discovery: "The identity provider could not be reached. Check the server logs.",
  token: "The identity provider response could not be verified. Check the server logs.",
};

function restoreNotice(asNew: boolean): string {
  return asNew ? "Version restored as a new file" : "Version restored in place; the previous content was snapshotted first";
}

async function versionAction(
  c: Context,
  body: Record<string, string | File>,
  run: () => Promise<unknown>,
): Promise<Response> {
  try {
    await run();
    return c.redirect(selectedFiles(field(body, "parentPath"), field(body, "id")), 303);
  } catch (err) {
    return c.redirect(selectedFiles(field(body, "parentPath"), field(body, "id"), errorMessage(err)), 303);
  }
}

function loginError(code: string | undefined, retry: string | undefined, reason: string | undefined): string | undefined {
  if (code === "invalid") return "Invalid admin token";
  if (code === "throttled") {
    const wait = formatRetryAfter(Number(retry));
    return wait ? `Too many sign-in attempts. Try again in ${wait}.` : "Too many sign-in attempts. Try again later.";
  }
  if (code === "oidc") {
    return (reason && oidcErrors[reason]) || "Single sign-on failed. Check the server logs.";
  }
  return undefined;
}

function setPage(c: { get(key: "page"): { title?: string; theme?: AdminTheme }; req: { header(name: string): string | undefined } }, title: string) {
  const page = c.get("page");
  page.title = title;
  page.theme = readThemeFromCookieHeader(c.req.header("cookie"));
}

async function loadBase(): Promise<{ stats: StatsResponse; roots: Node[]; health: HealthStatus; error?: string }> {
  const health = await loadHealth();
  try {
    const [stats, roots] = await Promise.all([loadStats(), loadRoots()]);
    return { stats, roots, health };
  } catch (err) {
    return { stats: emptyStats, roots: [], health: health === "ok" ? "fail" : health, error: errorMessage(err) };
  }
}

/**
 * Real service health for the topbar indicator, which used to be a hardcoded
 * green dot that stayed green through a total outage. Filegate answers 503 with
 * a body when a check fails, so a thrown error here means unreachable, not
 * merely degraded.
 */
async function loadHealth(): Promise<HealthStatus> {
  try {
    return (await client().system.health()).status;
  } catch {
    return "fail";
  }
}

async function loadFiles(path: string, selectedId: string) {
  const base = await loadBase();
  if (base.error) return { ...base, crumbs: buildCrumbs(""), children: [] as Node[] };
  try {
    if (!path.trim()) {
      return { stats: base.stats, health: base.health, crumbs: buildCrumbs(""), children: base.roots };
    }

    let current = await getNodeByPath(path);
    let selected: Node | undefined;
    if (current.type === "file") {
      selected = current;
      current = await getNodeByPath(parentPath(current.path));
    }
    if (selectedId) selected = await client().nodes.get(selectedId, { computeRecursiveSizes: true });
    const target = selected ?? current;
    return {
      stats: base.stats,
      health: base.health,
      crumbs: buildCrumbs(current.path),
      current,
      selected: target,
      ...(await loadAllChildren(current).then((listing) => ({ children: listing.children, truncated: listing.truncated }))),
      versions: await loadVersions(target),
    };
  } catch (err) {
    return { ...base, crumbs: buildCrumbs(""), children: base.roots, error: errorMessage(err) };
  }
}

/**
 * Version history for a file.
 *
 * Every versions endpoint answers 404 "versioning not supported on this mount"
 * when the mount cannot do it, which the SDK documents as the capability check,
 * so that case is reported as unsupported rather than as an error.
 */
async function loadVersions(node?: Node): Promise<{ items?: VersionResponse[]; unsupported?: boolean } | undefined> {
  if (!node || node.type !== "file") return undefined;
  try {
    return { items: await client().versions.listAll(node.id) };
  } catch (err) {
    if (err instanceof FilegateError && err.status === 404) return { unsupported: true };
    return { items: [] };
  }
}

async function loadStats(): Promise<StatsResponse> {
  return client().stats.get();
}

async function loadActivity(query: ActivityQuery): Promise<ActivityListResponse | undefined> {
  try {
    return await client().activity.list({
      limit: query.pageSize,
      offset: (query.page - 1) * query.pageSize,
      q: query.q,
      operation: query.operation,
      outcome: query.outcome,
    });
  } catch {
    return undefined;
  }
}

function parseActivityQuery(raw: { q?: string; operation?: string; outcome?: string; page?: string }): ActivityQuery {
  const page = Number.parseInt(raw.page || "1", 10);
  return {
    q: raw.q?.trim() || "",
    operation: raw.operation?.trim() || "",
    outcome: raw.outcome === "succeeded" || raw.outcome === "failed" || raw.outcome === "skipped" ? raw.outcome : "",
    page: Number.isFinite(page) && page > 0 ? page : 1,
    pageSize: 25,
  };
}

async function loadRoots(): Promise<Node[]> {
  const roots = await client().paths.get("", { computeRecursiveSizes: true });
  return isList(roots) ? roots.items : [];
}

const listingPageSize = 200;
/**
 * Upper bound on children fetched for one directory view.
 *
 * The previous code took only the first page and rendered it as if it were the
 * whole directory, so anything past 100 entries silently vanished and the item
 * count lied about it. Following the cursor fixes that; this cap keeps a
 * directory with a million entries from stalling the page, and the caller
 * reports when it bites instead of quietly truncating again.
 */
const listingMaxChildren = 5000;

async function getNodeByPath(path: string): Promise<Node> {
  const out: Node | NodeListResponse = await client().paths.get(path, { pageSize: listingPageSize, computeRecursiveSizes: true });
  if (isList(out)) throw new Error("path required");
  return out;
}

/** Walks the child cursor so the view reflects the whole directory. */
async function loadAllChildren(node: Node): Promise<{ children: Node[]; truncated: boolean }> {
  const children = [...(node.children ?? [])];
  let cursor = node.nextCursor;

  while (cursor && children.length < listingMaxChildren) {
    const page: Node | NodeListResponse = await client().paths.get(node.path, {
      pageSize: listingPageSize,
      cursor,
      computeRecursiveSizes: true,
    });
    if (isList(page)) break;
    children.push(...(page.children ?? []));
    if (!page.nextCursor || page.nextCursor === cursor) break;
    cursor = page.nextCursor;
  }

  return { children, truncated: !!cursor && children.length >= listingMaxChildren };
}

function buildCrumbs(path: string): Crumb[] {
  const clean = path.trim().replace(/^\/+|\/+$/g, "");
  const crumbs: Crumb[] = [{ name: "Mount roots" }];
  if (!clean) return crumbs;
  let acc = "";
  for (const part of clean.split("/")) {
    acc = acc ? `${acc}/${part}` : part;
    crumbs.push({ name: part, path: acc });
  }
  return crumbs;
}

function field(body: Record<string, string | File>, key: string): string {
  const value = body[key];
  return typeof value === "string" ? value.trim() : "";
}

function intField(body: Record<string, string | File>, key: string): number | undefined {
  const value = field(body, key);
  if (!value) return undefined;
  const out = Number(value);
  if (!Number.isInteger(out) || out < 0) throw new Error(`${key} must be a non-negative integer`);
  return out;
}

function ownershipFromForm(body: Record<string, string | File>): Ownership {
  const uid = intField(body, "uid");
  const gid = intField(body, "gid");
  if ((uid === undefined) !== (gid === undefined)) throw new Error("uid and gid must be set together");
  return {
    uid,
    gid,
    mode: field(body, "mode") || undefined,
    dirMode: field(body, "dirMode") || undefined,
  };
}

function conflictMode(value: string): FileConflictMode {
  return value === "rename" || value === "overwrite" ? value : "error";
}

function mkdirConflictMode(value: string): MkdirConflictMode {
  return value === "skip" || value === "rename" ? value : "error";
}

function queryError(query?: string, fallback?: string): string | undefined {
  return query || fallback || undefined;
}

type AdminUploadAllowItem = Pick<
  BrowserUploadAllowItem,
  "id" | "kind" | "path" | "size" | "checksum" | "segmentSize" | "contentType"
>;

async function createUploadSessions(
  uploads: AdminUploadAllowItem[],
  onConflict: BrowserUploadConflictMode,
): Promise<BrowserUploadAllowResult[]> {
  const out: BrowserUploadAllowResult[] = [];
  const sessions: AdminUploadAllowItem[] = [];
  for (const upload of uploads) {
    const existing = await resolveUploadConflict(upload, onConflict);
    if (existing) {
      out.push(existing);
      continue;
    }
    if (upload.size === 0) {
      try {
        const node = await client().paths.put(upload.path, new Uint8Array(), {
          contentType: upload.contentType,
          onConflict: fileWriteConflict(onConflict),
        });
        out.push({ id: upload.id, ok: true, node: node.node });
      } catch (err) {
        out.push(await conflictResult(upload, onConflict, err));
      }
      continue;
    }
    if (upload.kind === "direct") {
      try {
        const direct: DirectUploadURLResponse = await client().uploads.createDirectUploadURL({
          path: upload.path,
          contentType: upload.contentType,
          maxBytes: upload.size,
          onConflict: fileWriteConflict(onConflict),
        });
        out.push({ id: upload.id, ok: true, upload: { kind: "direct", direct } });
      } catch (err) {
        out.push(await conflictResult(upload, onConflict, err));
      }
    } else {
      if (onConflict === "rename") {
        out.push({ id: upload.id, ok: false, error: "rename is not supported for resumable uploads", code: "unsupported_conflict_mode" });
        continue;
      }
      sessions.push(upload);
    }
  }
  if (!sessions.length) return out;

  const direct: UploadSessionDirectRequest = { allow: ["putSegment", "status", "commit", "abort"] };
  const requests = sessions.map((upload) => uploadSessionRequest(upload, onConflict));
  try {
    const created = await client().uploads.sessions.createBatch({
      uploads: requests,
      segmentSize: sessions[0]?.segmentSize,
      direct,
    });
    created.sessions.forEach((session, index) => {
      out.push({ id: sessions[index].id, ok: true, upload: { kind: "session", session } });
    });
  } catch {
    for (const upload of sessions) {
      try {
        const session = await client().uploads.sessions.create({ ...uploadSessionRequest(upload, onConflict), direct });
        out.push({ id: upload.id, ok: true, upload: { kind: "session", session } });
      } catch (err) {
        out.push(await conflictResult(upload, onConflict, err));
      }
    }
  }
  return out;
}

async function resolveUploadConflict(
  upload: AdminUploadAllowItem,
  onConflict: BrowserUploadConflictMode,
): Promise<BrowserUploadAllowResult | undefined> {
  if (onConflict !== "skip-existing" && onConflict !== "skip-identical") return undefined;
  const fingerprint = onConflict === "skip-identical" ? "ensure" : "cached";
  try {
    const node = await client().paths.get(upload.path, { fingerprint });
    if (isList(node)) return undefined;
    if (onConflict === "skip-existing") return skipped(upload, "path already exists", "already_exists", node);
    if (node.type === "file" && upload.checksum && node.size === upload.size && node.sha256 === upload.checksum) {
      return skipped(upload, "identical file already exists", "identical_exists", node);
    }
    return { id: upload.id, ok: false, error: "path already exists", code: "already_exists", node };
  } catch (err) {
    if (err instanceof FilegateError && err.status === 404) return undefined;
    throw err;
  }
}

async function conflictResult(
  upload: AdminUploadAllowItem,
  onConflict: BrowserUploadConflictMode,
  err: unknown,
): Promise<BrowserUploadAllowResult> {
  if (err instanceof FilegateError && err.status === 409) {
    const resolved = await resolveUploadConflict(upload, onConflict);
    if (resolved) return resolved;
    return { id: upload.id, ok: false, error: "path already exists", code: "already_exists" };
  }
  return { id: upload.id, ok: false, error: errorMessage(err) };
}

function skipped(upload: AdminUploadAllowItem, error: string, code: string, node: Node): BrowserUploadAllowResult {
  return { id: upload.id, ok: false, skipped: true, error, code, node };
}

function fileWriteConflict(onConflict: BrowserUploadConflictMode): FileConflictMode {
  if (onConflict === "overwrite" || onConflict === "rename") return onConflict;
  return "error";
}

function uploadSessionConflict(onConflict: BrowserUploadConflictMode): "error" | "overwrite" {
  return onConflict === "overwrite" ? "overwrite" : "error";
}

function uploadSessionRequest(upload: AdminUploadAllowItem, onConflict: BrowserUploadConflictMode): UploadSessionCreateRequest {
  if (typeof upload.checksum !== "string" || !upload.checksum.startsWith("sha256:")) throw new Error("checksum is required");
  if (typeof upload.segmentSize !== "number" || upload.segmentSize <= 0) throw new Error("segmentSize must be > 0");
  return {
    path: upload.path,
    size: upload.size,
    checksum: upload.checksum,
    segmentSize: upload.segmentSize,
    contentType: upload.contentType,
    onConflict: uploadSessionConflict(onConflict),
  };
}

function parseUploadAllowRequest(body: unknown): Pick<BrowserUploadAllowRequest, "onConflict"> & { uploads: AdminUploadAllowItem[] } {
  const raw = (body as { uploads?: unknown })?.uploads;
  if (!Array.isArray(raw) || raw.length === 0) throw new Error("uploads required");
  const onConflict = uploadConflictMode((body as { onConflict?: unknown })?.onConflict);
  const uploads = raw.map((item) => {
    const upload = item as Partial<AdminUploadAllowItem>;
    if (typeof upload.id !== "string" || !upload.id.trim()) throw new Error("id is required");
    if (upload.kind !== "direct" && upload.kind !== "session") throw new Error("kind is required");
    if (typeof upload.path !== "string" || !upload.path.trim()) throw new Error("path is required");
    if (typeof upload.size !== "number" || upload.size < 0) throw new Error("size must be >= 0");
    if (upload.kind === "session") {
      if (typeof upload.checksum !== "string" || !upload.checksum.startsWith("sha256:")) throw new Error("checksum is required");
      if (typeof upload.segmentSize !== "number" || upload.segmentSize <= 0) throw new Error("segmentSize must be > 0");
    }
    return {
      id: upload.id.trim(),
      kind: upload.kind,
      path: upload.path.trim(),
      size: upload.size,
      checksum: typeof upload.checksum === "string" ? upload.checksum.trim() : undefined,
      segmentSize: upload.segmentSize,
      contentType: typeof upload.contentType === "string" ? upload.contentType : undefined,
    };
  });
  return { onConflict, uploads };
}

function uploadConflictMode(value: unknown): BrowserUploadConflictMode {
  switch (value) {
    case "skip-identical":
    case "error":
    case "overwrite":
    case "rename":
      return value;
    case "skip-existing":
    case undefined:
    case null:
    case "":
      return "skip-existing";
    default:
      throw new Error("invalid upload conflict mode");
  }
}

export function runtimeEnv() {
  return env();
}
