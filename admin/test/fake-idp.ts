import { exportJWK, generateKeyPair, SignJWT } from "jose";

/**
 * Minimal OpenID provider for tests: discovery, JWKS and a token endpoint.
 *
 * The token endpoint mints whatever the test asks for, because the desired
 * claims are encoded into the authorization code. That keeps the real code path
 * intact -- discovery, code exchange, JWKS signature verification, nonce and
 * group checks all run for real -- while letting a test produce a wrong nonce or
 * a token signed by an unknown key.
 */
export type FakeClaims = {
  sub?: string;
  email?: string;
  name?: string;
  groups?: string[] | string;
  nonce?: string;
  /** Sign with a key that is not in the published JWKS. */
  signWithUnknownKey?: boolean;
  /** Override the audience to something other than the client id. */
  audience?: string;
  /** Return a token response with no id_token at all. */
  omitIdToken?: boolean;
};

export function encodeClaims(claims: FakeClaims): string {
  return Buffer.from(JSON.stringify(claims), "utf8").toString("base64url");
}

export type FakeIdp = {
  issuer: string;
  clientId: string;
  clientSecret: string;
  /** Auth methods advertised in discovery; defaults to basic only. */
  tokenRequests: { authorization?: string; body: Record<string, string> }[];
  stop(): void;
};

export async function startFakeIdp(options: { authMethods?: string[]; port?: number; user?: FakeClaims } = {}): Promise<FakeIdp> {
  const published = await generateKeyPair("RS256");
  const unknown = await generateKeyPair("RS256");
  const kid = "test-key-1";
  const jwk = { ...(await exportJWK(published.publicKey)), kid, alg: "RS256", use: "sig" };

  const clientId = "filegate-admin-test";
  const clientSecret = "client-secret";
  const tokenRequests: FakeIdp["tokenRequests"] = [];

  const server = Bun.serve({
    port: options.port ?? 0,
    fetch: async (req) => {
      const url = new URL(req.url);
      const issuer = `http://127.0.0.1:${server.port}`;

      if (url.pathname === "/.well-known/openid-configuration") {
        return Response.json({
          issuer,
          authorization_endpoint: `${issuer}/authorize`,
          token_endpoint: `${issuer}/token`,
          jwks_uri: `${issuer}/jwks`,
          token_endpoint_auth_methods_supported: options.authMethods ?? ["client_secret_basic"],
        });
      }

      if (url.pathname === "/jwks") {
        return Response.json({ keys: [jwk] });
      }

      // Consent-free authorization endpoint: approves immediately and encodes
      // the claims to mint into the authorization code. Used for driving the
      // whole flow through a real browser; the tests build the code themselves.
      if (url.pathname === "/authorize") {
        const redirectUri = url.searchParams.get("redirect_uri");
        if (!redirectUri) return new Response("missing redirect_uri", { status: 400 });

        const code = encodeClaims({
          nonce: url.searchParams.get("nonce") ?? undefined,
          ...(options.user ?? { sub: "dev-user", email: "dev@example.com", groups: ["filegate-admins"] }),
        });
        const back = new URL(redirectUri);
        back.searchParams.set("code", code);
        back.searchParams.set("state", url.searchParams.get("state") ?? "");
        return Response.redirect(back.toString(), 302);
      }

      if (url.pathname === "/token" && req.method === "POST") {
        const body = Object.fromEntries(new URLSearchParams(await req.text()));
        tokenRequests.push({ authorization: req.headers.get("authorization") ?? undefined, body });

        let claims: FakeClaims;
        try {
          claims = JSON.parse(Buffer.from(body.code ?? "", "base64url").toString("utf8"));
        } catch {
          return Response.json({ error: "invalid_grant" }, { status: 400 });
        }

        if (claims.omitIdToken) return Response.json({ access_token: "irrelevant", token_type: "Bearer" });

        const { sub = "user-1", email, name, groups, nonce, audience, signWithUnknownKey } = claims;
        const payload: Record<string, unknown> = { nonce };
        if (email) payload.email = email;
        if (name) payload.name = name;
        if (groups !== undefined) payload.groups = groups;

        const idToken = await new SignJWT(payload)
          .setProtectedHeader({ alg: "RS256", kid })
          .setIssuedAt()
          .setIssuer(issuer)
          .setSubject(sub)
          .setAudience(audience ?? clientId)
          .setExpirationTime("5m")
          .sign(signWithUnknownKey ? unknown.privateKey : published.privateKey);

        return Response.json({ id_token: idToken, access_token: "irrelevant", token_type: "Bearer" });
      }

      return new Response("not found", { status: 404 });
    },
  });

  return {
    issuer: `http://127.0.0.1:${server.port}`,
    clientId,
    clientSecret,
    tokenRequests,
    stop: () => server.stop(true),
  };
}
