/** Local browser contract fixture, not a Filegate server. No external credentials. */
const sdk = Bun.file(new URL("../../dist/utils.js", import.meta.url));
if (!await sdk.exists()) throw new Error("Run bun run build in sdk/ts first");

interface Observation {
  method: string;
  path: string;
  query: string;
  origin: string | null;
  authorization: string | null;
  cookie: string | null;
  headers: string[];
  body: string;
  time: number;
}
const requests: Observation[] = [];
const status = { id: "browser-contract", root: "test", state: "open", size: 3, chunkSize: 3, received: 3, uploadedSegments: 1 };
let pageOrigin = "";
const transfer = Bun.serve({ hostname: "127.0.0.1", port: 0, async fetch(request) {
  const url = new URL(request.url);
  const headers = {
    "Access-Control-Allow-Origin": pageOrigin,
    "Access-Control-Allow-Methods": "GET, PUT, DELETE, OPTIONS",
    "Access-Control-Allow-Headers": "Content-Type",
    "Access-Control-Expose-Headers": "Retry-After",
    "Cache-Control": "no-store",
  };
  if (url.pathname === "/observations") return Response.json(requests, { headers });
  requests.push({ method: request.method, path: url.pathname, query: url.search,
    origin: request.headers.get("origin"), authorization: request.headers.get("authorization"),
    cookie: request.headers.get("cookie"), headers: [...request.headers.keys()],
    body: await request.text(), time: Date.now() });
  if (request.method === "OPTIONS") return new Response(null, { status: 204, headers });
  const attempts = requests.filter(row => row.path === url.pathname && row.method !== "OPTIONS").length;
  const retry = url.pathname === "/retry" && attempts < 3 || ["/busy", "/cancel", "/long"].includes(url.pathname);
  if (retry) return Response.json({ error: "busy", message: "Fixture capacity reached" }, {
    status: 503, headers: { ...headers, "Retry-After": url.pathname === "/long" ? "60" : url.pathname === "/cancel" ? "2" : "1" },
  });
  return Response.json(status, { headers });
} });
const page = Bun.serve({ hostname: "127.0.0.1", port: 0, async fetch(request) {
  const url = new URL(request.url);
  if (url.pathname === "/v1/direct/fixture.scope") return Response.json({
    method: request.method, cookie: request.headers.get("cookie"),
    authorization: request.headers.get("authorization"), body: await request.text(),
  }, { headers: { "Cache-Control": "no-store" } });
  if (url.pathname === "/utils.js") return new Response(sdk, { headers: { "Content-Type": "text/javascript" } });
  if (url.pathname === "/index.js") return new Response(Bun.file(new URL("../../dist/index.js", import.meta.url)), { headers: { "Content-Type": "text/javascript" } });
  if (url.pathname === "/types.js") return new Response(Bun.file(new URL("../../dist/types.js", import.meta.url)), { headers: { "Content-Type": "text/javascript" } });
  if (url.pathname === "/browser.js") return new Response(Bun.file(new URL("./browser.js", import.meta.url)), { headers: { "Content-Type": "text/javascript" } });
  return new Response(`<!doctype html><meta charset="utf-8"><title>Filegate SDK browser contracts</title>
    <h1>Filegate SDK browser contracts</h1><p>Real browser; local simulated transfer endpoint.</p>
    <pre id="result">Running…</pre><script>globalThis.transferOrigin=${JSON.stringify(transfer.url.origin)}</script>
    <script type="module" src="/browser.js"></script>`, { headers: { "Content-Type": "text/html" } });
} });
pageOrigin = page.url.origin;
console.log(JSON.stringify({ page: page.url.href, transfer: transfer.url.href }));
for (const signal of ["SIGINT", "SIGTERM"] as const) process.on(signal, () => { page.stop(true); transfer.stop(true); process.exit(0); });
