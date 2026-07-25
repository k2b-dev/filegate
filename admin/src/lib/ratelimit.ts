import type { RateLimiter } from "@valentinkolb/sync";
import { env } from "./env";

const LOGIN_LIMIT = 10;
const LOGIN_WINDOW_SECONDS = 5 * 60;

/**
 * The two @valentinkolb/sync entrypoints expose an identical rate-limit API, so
 * the backend is a single import decision:
 *
 * - REDIS_URL set   -> the server build, backed by Bun's Redis client, shared
 *                      across replicas.
 * - REDIS_URL unset -> the in-memory build, per process. Correct for a single
 *                      instance and the sane default; with several replicas the
 *                      effective limit multiplies by the replica count.
 */
async function createLoginLimiter(): Promise<RateLimiter> {
  const { redisUrl } = env();
  const mod = redisUrl
    ? await import("@valentinkolb/sync")
    : await import("@valentinkolb/sync/browser");

  console.log(`[filegate-admin] login rate limit: ${LOGIN_LIMIT} attempts per ${LOGIN_WINDOW_SECONDS}s per client (${redisUrl ? "redis" : "in-memory"})`);

  return mod.ratelimit({
    id: "admin-login",
    limit: LOGIN_LIMIT,
    windowSecs: LOGIN_WINDOW_SECONDS,
  });
}

let pending: Promise<RateLimiter> | undefined;

function loginLimiter(): Promise<RateLimiter> {
  pending ??= createLoginLimiter();
  return pending;
}

export type LoginAttemptVerdict = {
  limited: boolean;
  /** Seconds until the caller may try again; only meaningful when limited. */
  retryAfterSeconds: number;
};

/** Counts one login attempt against the client's window. */
export async function recordLoginAttempt(clientId: string): Promise<LoginAttemptVerdict> {
  const limiter = await loginLimiter();
  const result = await limiter.check(clientId);
  return {
    limited: result.limited,
    retryAfterSeconds: Math.max(1, Math.ceil(result.resetIn / 1000)),
  };
}
