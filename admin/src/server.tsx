import { app, runtimeEnv } from "./app";

const { port } = runtimeEnv();

Bun.serve({
  port,
  // The server object is handed to Hono as the request env so handlers can read
  // the socket address for rate limiting; see lib/request.ts.
  fetch: (req, server) => app.fetch(req, { server }),
});

console.log(`filegate-admin listening on :${port}`);
