import { DirectSession, putDirect, archiveRaw } from "/utils.js";
import { Filegate } from "/index.js";

const origin = globalThis.transferOrigin;
const assertions = [];
const assert = (value, message) => { if (!value) throw new Error(message); assertions.push(message); };
const delay = milliseconds => new Promise(resolve => setTimeout(resolve, milliseconds));
const observations = async () => (await fetch(`${origin}/observations`)).json();
const dataRequests = (rows, path) => rows.filter(row => row.path === path && row.method !== "OPTIONS");
const session = path => new DirectSession(`${origin}${path}?lease=fixture-signed-scope`);
const failure = async promise => { try { await promise; } catch (error) { return error; } throw new Error("Expected rejection"); };

try {
  document.cookie = "filegate_browser_contract=present; Path=/; SameSite=Lax";
  const sameOriginURL = `${location.origin}/v1/direct/fixture.scope`;
  const control = await (await fetch(sameOriginURL)).json();
  assert(control.cookie?.includes("filegate_browser_contract=present"), "Control: native same-origin fetch sends the installed browser cookie");
  const sameSession = new DirectSession(sameOriginURL);
  const sameOriginRequests = [
    ["DirectSession GET", await sameSession.status()],
    ["DirectSession PUT", await sameSession.put(0, new Blob(["abc"]))],
    ["putDirect", await putDirect(sameOriginURL, new Blob(["abc"]))],
    ["archiveRaw", await (await archiveRaw({ url: sameOriginURL, method: "POST", expires: "2099-01-01T00:00:00Z", manifest: "fixture-manifest" })).json()],
    ["Filegate.downloadRaw", await (await new Filegate({ baseUrl: location.origin, token: "fixture-backend-token" }).downloadRaw({ url: sameOriginURL, method: "GET", expires: "2099-01-01T00:00:00Z" })).json()],
  ];
  for (const [method, row] of sameOriginRequests) {
    assert(row.cookie === null && row.authorization === null, `${method}: same-origin transfer omits browser cookies and Authorization`);
  }
  document.cookie = "filegate_browser_contract=; Path=/; Max-Age=0";
  assert(location.origin !== origin, "Page and transfer endpoint have different origins");
  assert((await session("/status").status()).state === "open", "Native browser fetch receiver works for cross-origin GET");
  assert((await session("/put").put(0, new Blob(["abc"], { type: "application/octet-stream" }))).received === 3,
    "Cross-origin Blob PUT succeeds through browser CORS preflight");
  assert((await session("/retry").put(0, new Blob(["abc"]))).received === 3, "Replayable Blob succeeds after two 503 responses");
  const busyError = await failure(session("/busy").put(0, new Blob(["abc"])));
  assert(busyError.status === 503, "Repeated 503 returns structured failure after bounded retries");
  const longError = await failure(session("/long").status());
  assert(longError.status === 503, "Long Retry-After is returned without an early retry");

  const controller = new AbortController();
  const cancelled = failure(session("/cancel").put(0, new Blob(["abc"]), controller.signal));
  const deadline = performance.now() + 5000;
  while (dataRequests(await observations(), "/cancel").length === 0) {
    if (performance.now() > deadline) throw new Error("Abort fixture request was not observed");
    await delay(10);
  }
  controller.abort(new Error("browser cancellation"));
  assert((await cancelled).message === "browser cancellation", "Abort rejects a pending retry immediately");
  await delay(2200);
  const rows = await observations();
  assert(dataRequests(rows, "/cancel").length === 1, "Abort prevents every later write beyond the Retry-After deadline");
  assert(dataRequests(rows, "/long").length === 1, "Long Retry-After causes exactly one request");
  for (const path of ["/retry", "/busy"]) {
    const attempts = dataRequests(rows, path);
    assert(attempts.length === 3, `${path}: exactly three total attempts`);
    assert(attempts.every(row => row.method === "PUT" && row.body === "abc"), `${path}: retries preserve method and every body byte`);
    assert(attempts.slice(1).every((row, index) => row.time - attempts[index].time >= 950), `${path}: retries respect exposed Retry-After`);
  }
  assert(rows.some(row => row.path === "/put" && row.method === "OPTIONS"), "Browser issued an actual PUT preflight");
  assert(rows.every(row => row.origin === location.origin), "All transfer requests carry the actual page origin");
  assert(rows.every(row => row.authorization === null && row.cookie === null && !row.headers.includes("x-filegate-execution")),
    "No Authorization, cookie, or backend execution identity reaches the transfer target");
  assert(rows.filter(row => row.method !== "OPTIONS").every(row => new URLSearchParams(row.query).get("lease") === "fixture-signed-scope"),
    "All operations preserve the issued lease scope");
  globalThis.browserContractResult = { passed: true, userAgent: navigator.userAgent, assertions, sameOriginRequests, requests: rows };
} catch (error) {
  globalThis.browserContractResult = { passed: false, userAgent: navigator.userAgent, assertions, error: String(error?.stack ?? error) };
}
document.querySelector("#result").textContent = JSON.stringify(globalThis.browserContractResult, null, 2);
