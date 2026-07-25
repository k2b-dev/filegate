import type { JSX } from "solid-js";

type LayoutProps = {
  active: "overview" | "files" | "search" | "system";
  title: string;
  description: string;
  mounts: number;
  /** Real service health. Absent means unknown, shown as such rather than green. */
  health?: "ok" | "degraded" | "fail";
  notice?: string;
  error?: string;
  children: JSX.Element;
};

// The indicator used to be a hardcoded green dot that stayed green through a
// total outage, which is worse than showing nothing.
function healthLabel(status?: string): string {
  if (status === "ok") return "Healthy";
  if (status === "degraded") return "Degraded";
  if (status === "fail") return "Unreachable";
  return "Unknown";
}

function healthClass(status?: string): string {
  if (status === "ok") return "ok";
  if (status === "degraded") return "warn";
  return "bad";
}

function healthTitle(status?: string): string {
  if (status === "degraded") return "A dependency check is failing; see the System page";
  if (status === "fail") return "Filegate is unreachable or a dependency failed";
  if (status === "ok") return "All dependency checks pass";
  return "Health could not be determined";
}

const nav = [
  ["overview", "Overview", "/"],
  ["files", "Files", "/files"],
  ["search", "Search", "/search"],
  ["system", "System", "/system"],
] as const;

export function Layout(props: LayoutProps) {
  return (
    <>
      <header class="topbar" style="view-transition-name: fg-topbar">
        <div class="service">Filegate Admin</div>
        <div class="top-meta">
          <span class={`status ${healthClass(props.health)}`} title={healthTitle(props.health)}>
            <span class="dot" />
            {healthLabel(props.health)}
          </span>
          <span>
            {props.mounts} mount{props.mounts === 1 ? "" : "s"}
          </span>
          <button class="btn theme-toggle" type="button" data-theme-toggle aria-label="Toggle theme">
            Theme
          </button>
          <form method="post" action="/logout">
            <button class="btn" type="submit">
              Log out
            </button>
          </form>
        </div>
      </header>
      <div class="shell">
        <aside class="sidebar" style="view-transition-name: fg-sidebar">
          <div class="side-title">Resources</div>
          <nav class="nav">
            {nav.map(([key, label, href]) => (
              <a class={props.active === key ? "active" : ""} href={href}>
                {label}
              </a>
            ))}
          </nav>
        </aside>
        <main class="main">
          <nav class="breadcrumbs">
            <span>Filegate</span>
            <span>/</span>
            <strong>{props.title}</strong>
          </nav>
          {props.notice && <div class="notice">{props.notice}</div>}
          {props.error && <div class="error">{props.error}</div>}
          <section class="head" style="view-transition-name: fg-page-head">
            <div>
              <h1>{props.title}</h1>
              <div class="desc">{props.description}</div>
            </div>
            {props.active === "system" && (
              <form method="post" action="/system/rescan" data-confirm-rescan>
                <button class="btn primary">Rescan index</button>
              </form>
            )}
          </section>
          {props.children}
        </main>
      </div>
      <script src="/prompts.js" defer />
    </>
  );
}

export function LoginPage(props: { error?: string; methods: { token: boolean; oidc: boolean } }) {
  return (
    <main class="login">
      <div class="panel" style="view-transition-name: fg-login-panel">
        <div class="panel-head">
          <h2>Filegate Admin</h2>
        </div>
        <div class="panel-body">
          {props.error && <div class="error">{props.error}</div>}
          {props.methods.oidc && (
            <a class="btn primary login-sso" href="/auth/login">
              Sign in with single sign-on
            </a>
          )}
          {props.methods.oidc && props.methods.token && <div class="login-divider">or</div>}
          {props.methods.token && (
            <form method="post" action="/login" class="form-stack">
              <div class="field">
                <label for="admin-token">Admin token</label>
                <input
                  id="admin-token"
                  class="input"
                  name="token"
                  type="password"
                  autocomplete="current-password"
                  autofocus={!props.methods.oidc}
                />
              </div>
              <button class={props.methods.oidc ? "btn" : "btn primary"} type="submit">
                Sign in
              </button>
            </form>
          )}
        </div>
      </div>
    </main>
  );
}
