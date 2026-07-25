import type { Context } from "hono";
import { getCookie } from "hono/cookie";

export type FileView = "list" | "grid";

export const viewCookieName = "filegate_admin_view";
export const viewCookieMaxAge = 365 * 24 * 60 * 60;

function isView(value: string | undefined): value is FileView {
  return value === "list" || value === "grid";
}

/**
 * Resolves the files layout for this request.
 *
 * An explicit ?view= wins, otherwise the stored preference applies. The cookie
 * is written in the browser (see theme.ts) rather than here: the SSR handler
 * builds its own Response, so a cookie set on the Hono context never reaches
 * the client. The same constraint is why the theme cookie is client-written.
 */
export function resolveFileView(c: Context): FileView {
  const requested = c.req.query("view");
  if (isView(requested)) return requested;

  const stored = getCookie(c, viewCookieName);
  return isView(stored) ? stored : "list";
}
