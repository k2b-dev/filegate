import { AsyncLocalStorage } from "node:async_hooks";
import type { MiddlewareHandler } from "hono";
import { currentSession } from "./auth";

/**
 * Request-scoped identity of the signed-in admin.
 *
 * Filegate attributes every audit entry to the bearer token, so with a single
 * shared token several admins are indistinguishable in the log. It already
 * accepts an X-Filegate-Actor header and records it as delegatedActor, which is
 * what this carries.
 *
 * Async-local rather than a parameter because client() is called from route
 * handlers and from load helpers several frames deep; threading an actor
 * argument through all of them would touch every call site to move one string.
 */
const store = new AsyncLocalStorage<string>();

/** Label of the admin behind the current request, if any. */
export function currentActor(): string | undefined {
  return store.getStore();
}

/** Runs authenticated requests with the session label available to client(). */
export function withActor(): MiddlewareHandler {
  return async (c, next) => {
    const session = currentSession(c);
    if (!session) return next();
    return store.run(session.label, next);
  };
}
