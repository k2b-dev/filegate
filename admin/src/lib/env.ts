import { randomBytes } from "node:crypto";

export type CookieSecureMode = "auto" | "always" | "never";

export type AdminEnv = {
  filegateUrl: string;
  filegateToken: string;
  adminToken: string;
  sessionSecret: string;
  port: number;
  trustProxy: boolean;
  cookieSecure: CookieSecureMode;
  redisUrl?: string;
};

function required(name: string, hint: string): string {
  const value = Bun.env[name]?.trim();
  if (!value) throw new Error(`${name} is required: ${hint}`);
  return value;
}

function boolFlag(name: string): boolean {
  const value = Bun.env[name]?.trim().toLowerCase();
  return value === "1" || value === "true" || value === "yes";
}

function cookieSecureMode(): CookieSecureMode {
  const value = Bun.env.ADMIN_COOKIE_SECURE?.trim().toLowerCase();
  if (!value || value === "auto") return "auto";
  if (value === "1" || value === "true" || value === "yes") return "always";
  if (value === "0" || value === "false" || value === "no") return "never";
  throw new Error(`ADMIN_COOKIE_SECURE must be auto, true or false, got ${value}`);
}

// The session secret must stay stable for the process lifetime, otherwise every
// request would verify against a different key. When it is not configured we
// generate one and warn: sessions then survive neither a restart nor a second
// replica, which is fine for local use and wrong for a real deployment.
function resolveSessionSecret(): string {
  const configured = Bun.env.ADMIN_SESSION_SECRET?.trim();
  if (configured) return configured;
  console.warn(
    "[filegate-admin] ADMIN_SESSION_SECRET is not set; using a random secret. Sessions will not survive a restart and will not work across replicas.",
  );
  return randomBytes(32).toString("hex");
}

function resolve(): AdminEnv {
  const port = Number(Bun.env.PORT || 3000);
  if (!Number.isInteger(port) || port <= 0 || port > 65535) {
    throw new Error(`PORT must be a valid port number, got ${Bun.env.PORT}`);
  }

  const cfg: AdminEnv = {
    filegateUrl: required("FILEGATE_URL", "REST API base URL of the Filegate server"),
    filegateToken: required("FILEGATE_TOKEN", "Filegate bearer token, kept server-side"),
    // Deliberately no fallback to FILEGATE_TOKEN. Sharing them means brute
    // forcing the admin login yields the Filegate master credential.
    adminToken: required("ADMIN_TOKEN", "admin login token; must differ from FILEGATE_TOKEN"),
    sessionSecret: resolveSessionSecret(),
    port,
    trustProxy: boolFlag("ADMIN_TRUST_PROXY"),
    cookieSecure: cookieSecureMode(),
    redisUrl: Bun.env.REDIS_URL?.trim() || undefined,
  };

  if (cfg.adminToken === cfg.filegateToken) {
    throw new Error("ADMIN_TOKEN must differ from FILEGATE_TOKEN so the admin login cannot leak the Filegate master token");
  }
  if (cfg.sessionSecret === cfg.filegateToken || cfg.sessionSecret === cfg.adminToken) {
    throw new Error("ADMIN_SESSION_SECRET must differ from FILEGATE_TOKEN and ADMIN_TOKEN");
  }

  return cfg;
}

let cached: AdminEnv | undefined;

export function env(): AdminEnv {
  cached ??= resolve();
  return cached;
}
