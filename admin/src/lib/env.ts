import { randomBytes } from "node:crypto";

export type CookieSecureMode = "auto" | "always" | "never";

export type OidcSettings = {
  issuer: string;
  clientId: string;
  clientSecret: string;
  redirectUrl: string;
  scopes: string;
  groupsClaim: string;
  /** Empty means access control is delegated to the identity provider. */
  allowedGroups: string[];
};

export type AdminEnv = {
  filegateUrl: string;
  filegateToken: string;
  /** Undefined when only OIDC login is configured; the token form is then hidden. */
  adminToken?: string;
  sessionSecret: string;
  port: number;
  trustProxy: boolean;
  cookieSecure: CookieSecureMode;
  redisUrl?: string;
  oidc?: OidcSettings;
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

function list(name: string): string[] {
  return (Bun.env[name] ?? "")
    .split(",")
    .map((entry) => entry.trim())
    .filter(Boolean);
}

/**
 * OIDC is enabled by configuring it. All four core values must be present
 * together, so a half-filled configuration fails loudly instead of silently
 * falling back to token-only login.
 */
function resolveOidc(): OidcSettings | undefined {
  const core = {
    issuer: Bun.env.OIDC_ISSUER?.trim(),
    clientId: Bun.env.OIDC_CLIENT_ID?.trim(),
    clientSecret: Bun.env.OIDC_CLIENT_SECRET?.trim(),
    redirectUrl: Bun.env.OIDC_REDIRECT_URL?.trim(),
  };

  const provided = Object.entries(core).filter(([, value]) => !!value);
  if (provided.length === 0) return undefined;
  if (provided.length < 4) {
    const missing = Object.entries(core)
      .filter(([, value]) => !value)
      .map(([key]) => `OIDC_${key.replace(/[A-Z]/g, (c) => `_${c}`).toUpperCase()}`);
    throw new Error(`incomplete OIDC configuration; missing ${missing.join(", ")}`);
  }

  let issuerUrl: URL;
  try {
    issuerUrl = new URL(core.issuer!);
  } catch {
    throw new Error(`OIDC_ISSUER must be an absolute URL, got ${core.issuer}`);
  }
  if (issuerUrl.protocol !== "https:" && issuerUrl.hostname !== "localhost" && issuerUrl.hostname !== "127.0.0.1") {
    throw new Error("OIDC_ISSUER must use https outside of localhost");
  }

  const scopes = Bun.env.OIDC_SCOPES?.trim() || "openid profile email";
  return {
    issuer: core.issuer!,
    clientId: core.clientId!,
    clientSecret: core.clientSecret!,
    redirectUrl: core.redirectUrl!,
    // openid is not optional; add it back rather than failing over a typo.
    scopes: scopes.split(/\s+/).includes("openid") ? scopes : `openid ${scopes}`,
    groupsClaim: Bun.env.OIDC_GROUPS_CLAIM?.trim() || "groups",
    allowedGroups: list("OIDC_ALLOWED_GROUPS"),
  };
}

function resolve(): AdminEnv {
  const port = Number(Bun.env.PORT || 3000);
  if (!Number.isInteger(port) || port <= 0 || port > 65535) {
    throw new Error(`PORT must be a valid port number, got ${Bun.env.PORT}`);
  }

  const oidc = resolveOidc();
  const cfg: AdminEnv = {
    filegateUrl: required("FILEGATE_URL", "REST API base URL of the Filegate server"),
    filegateToken: required("FILEGATE_TOKEN", "Filegate bearer token, kept server-side"),
    // Deliberately no fallback to FILEGATE_TOKEN. Sharing them means brute
    // forcing the admin login yields the Filegate master credential. Optional
    // once OIDC can log people in, where it stays useful as a break-glass path.
    adminToken: oidc
      ? Bun.env.ADMIN_TOKEN?.trim() || undefined
      : required("ADMIN_TOKEN", "admin login token; must differ from FILEGATE_TOKEN, or configure OIDC instead"),
    sessionSecret: resolveSessionSecret(),
    port,
    trustProxy: boolFlag("ADMIN_TRUST_PROXY"),
    cookieSecure: cookieSecureMode(),
    redisUrl: Bun.env.REDIS_URL?.trim() || undefined,
    oidc,
  };

  if (cfg.adminToken && cfg.adminToken === cfg.filegateToken) {
    throw new Error("ADMIN_TOKEN must differ from FILEGATE_TOKEN so the admin login cannot leak the Filegate master token");
  }
  if (cfg.sessionSecret === cfg.filegateToken || (cfg.adminToken && cfg.sessionSecret === cfg.adminToken)) {
    throw new Error("ADMIN_SESSION_SECRET must differ from FILEGATE_TOKEN and ADMIN_TOKEN");
  }

  if (oidc && oidc.allowedGroups.length === 0) {
    // Not an error: identity providers such as Authentik bind a group policy to
    // the client itself, which makes a second allowlist here duplicate
    // bookkeeping. It is loud because the failure mode of an open IdP in front
    // of an unrestricted admin panel is severe and silent.
    console.warn(
      "[filegate-admin] OIDC_ALLOWED_GROUPS is not set: any account your identity provider lets through this client becomes an admin. Access control is delegated to the IdP.",
    );
  }

  return cfg;
}

let cached: AdminEnv | undefined;

export function env(): AdminEnv {
  cached ??= resolve();
  return cached;
}
