import { createHash, randomBytes } from "node:crypto";
import { createRemoteJWKSet, jwtVerify, type JWTPayload } from "jose";
import { env } from "./env";

export const oidcStateCookieName = "filegate_admin_oidc";
export const oidcStateTtlSeconds = 10 * 60;

/** Short-lived per-attempt values that must survive the round trip to the IdP. */
export type OidcFlowState = {
  state: string;
  nonce: string;
  verifier: string;
};

export type OidcIdentity = {
  sub: string;
  label: string;
  groups: string[];
};

/** Carries a stable reason so the login page can say something useful. */
export class OidcError extends Error {
  constructor(
    readonly reason: string,
    message: string,
  ) {
    super(message);
    this.name = "OidcError";
  }
}

type Discovery = {
  issuer: string;
  authorization_endpoint: string;
  token_endpoint: string;
  jwks_uri: string;
  token_endpoint_auth_methods_supported?: string[];
};

// Discovery and the JWKS are cached for the process lifetime. A failed discovery
// clears the cache so the next attempt retries instead of poisoning every login
// until restart.
let discoveryCache: Promise<Discovery> | undefined;
const jwksCache = new Map<string, ReturnType<typeof createRemoteJWKSet>>();

async function discover(): Promise<Discovery> {
  discoveryCache ??= (async () => {
    const { issuer } = oidcRequired();
    const url = `${issuer.replace(/\/+$/, "")}/.well-known/openid-configuration`;

    const res = await fetch(url, { headers: { accept: "application/json" } });
    if (!res.ok) throw new OidcError("discovery", `OIDC discovery failed: ${url} returned ${res.status}`);

    const doc = (await res.json()) as Partial<Discovery>;
    for (const field of ["issuer", "authorization_endpoint", "token_endpoint", "jwks_uri"] as const) {
      if (typeof doc[field] !== "string" || !doc[field]) {
        throw new OidcError("discovery", `OIDC discovery document from ${url} is missing ${field}`);
      }
    }
    return doc as Discovery;
  })().catch((err) => {
    discoveryCache = undefined;
    throw err;
  });

  return discoveryCache;
}

function keys(jwksUri: string) {
  let set = jwksCache.get(jwksUri);
  if (!set) {
    set = createRemoteJWKSet(new URL(jwksUri));
    jwksCache.set(jwksUri, set);
  }
  return set;
}

function oidcRequired() {
  const settings = env().oidc;
  if (!settings) throw new OidcError("disabled", "OIDC is not configured");
  return settings;
}

function base64url(input: Buffer): string {
  return input.toString("base64url");
}

/**
 * Start a login. The returned flow state must be sealed into a cookie by the
 * caller; nothing is kept server-side, so this works across replicas unchanged.
 */
export async function beginLogin(): Promise<{ redirectTo: string; flow: OidcFlowState }> {
  const settings = oidcRequired();
  const discovery = await discover();

  const flow: OidcFlowState = {
    state: base64url(randomBytes(24)),
    nonce: base64url(randomBytes(24)),
    verifier: base64url(randomBytes(32)),
  };

  const params = new URLSearchParams({
    response_type: "code",
    client_id: settings.clientId,
    redirect_uri: settings.redirectUrl,
    scope: settings.scopes,
    state: flow.state,
    nonce: flow.nonce,
    code_challenge: base64url(createHash("sha256").update(flow.verifier).digest()),
    code_challenge_method: "S256",
  });

  return { redirectTo: `${discovery.authorization_endpoint}?${params}`, flow };
}

/**
 * Finish a login: verify the round trip, exchange the code, validate the ID
 * token and map its claims onto an identity.
 */
export async function completeLogin(input: { code: string; state: string; flow: OidcFlowState }): Promise<OidcIdentity> {
  const settings = oidcRequired();
  const discovery = await discover();

  if (!input.state || input.state !== input.flow.state) {
    throw new OidcError("state", "OIDC state mismatch; the login was not started by this browser");
  }

  const idToken = await exchangeCode(discovery, input.code, input.flow.verifier);
  const claims = await verifyIdToken(discovery, idToken, input.flow.nonce);
  const identity = toIdentity(claims, settings.groupsClaim);

  if (settings.allowedGroups.length > 0) {
    const allowed = identity.groups.some((group) => settings.allowedGroups.includes(group));
    if (!allowed) {
      throw new OidcError("group", `${identity.label} is not in an allowed group`);
    }
  }

  return identity;
}

async function exchangeCode(discovery: Discovery, code: string, verifier: string): Promise<string> {
  const settings = oidcRequired();
  const body = new URLSearchParams({
    grant_type: "authorization_code",
    code,
    redirect_uri: settings.redirectUrl,
    code_verifier: verifier,
  });
  const headers: Record<string, string> = { "content-type": "application/x-www-form-urlencoded", accept: "application/json" };

  // Prefer basic auth, which is the spec default, but fall back to form-encoded
  // credentials when the provider only advertises that.
  const supported = discovery.token_endpoint_auth_methods_supported;
  const usePost = Array.isArray(supported) && !supported.includes("client_secret_basic") && supported.includes("client_secret_post");
  if (usePost) {
    body.set("client_id", settings.clientId);
    body.set("client_secret", settings.clientSecret);
  } else {
    const credentials = `${encodeURIComponent(settings.clientId)}:${encodeURIComponent(settings.clientSecret)}`;
    headers.authorization = `Basic ${Buffer.from(credentials, "utf8").toString("base64")}`;
  }

  const res = await fetch(discovery.token_endpoint, { method: "POST", headers, body });
  if (!res.ok) {
    const detail = await res.text().catch(() => "");
    throw new OidcError("token", `token exchange failed with ${res.status}: ${detail.slice(0, 200)}`);
  }

  const payload = (await res.json()) as { id_token?: unknown };
  if (typeof payload.id_token !== "string" || !payload.id_token) {
    throw new OidcError("token", "token response did not contain an id_token");
  }
  return payload.id_token;
}

async function verifyIdToken(discovery: Discovery, idToken: string, nonce: string): Promise<JWTPayload> {
  const settings = oidcRequired();

  // The spec would allow skipping signature verification here, since the token
  // arrives directly from the token endpoint over TLS. We verify anyway: this is
  // the gate to an admin panel, and jose makes it a few lines.
  let payload: JWTPayload;
  try {
    ({ payload } = await jwtVerify(idToken, keys(discovery.jwks_uri), {
      issuer: discovery.issuer,
      audience: settings.clientId,
    }));
  } catch (err) {
    throw new OidcError("token", `id_token verification failed: ${err instanceof Error ? err.message : "unknown error"}`);
  }

  if (payload.nonce !== nonce) {
    throw new OidcError("nonce", "id_token nonce mismatch; the response does not belong to this login attempt");
  }
  if (typeof payload.sub !== "string" || !payload.sub) {
    throw new OidcError("token", "id_token has no subject");
  }

  return payload;
}

function firstString(...values: unknown[]): string | undefined {
  for (const value of values) {
    if (typeof value === "string" && value.trim()) return value.trim();
  }
  return undefined;
}

/** Providers disagree on group claim shape: an array, or one delimited string. */
function readGroups(claims: JWTPayload, claim: string): string[] {
  const raw = claims[claim];
  if (Array.isArray(raw)) return raw.filter((entry): entry is string => typeof entry === "string" && entry.trim() !== "");
  if (typeof raw === "string") return raw.split(/[,\s]+/).filter(Boolean);
  return [];
}

function toIdentity(claims: JWTPayload, groupsClaim: string): OidcIdentity {
  const sub = claims.sub as string;
  return {
    sub,
    label: firstString(claims.email, claims.preferred_username, claims.name) ?? sub,
    groups: readGroups(claims, groupsClaim),
  };
}
