# Conventions

Hard-won repo conventions. Each one exists because skipping it once caused a real bug or a painful review cycle.

## Error model

Domain returns sentinel errors from [`domain/errors.go`](../../../domain/errors.go):

```go
ErrNotFound, ErrConflict, ErrInvalidArgument, ErrForbidden, ErrInsufficientStorage
```

The HTTP adapter maps them via `statusFromErr` in `adapter/http/router.go`. New error classes must be added there too — otherwise they default to 500.

For 409 Conflict responses, prefer `writeConflict(w, msg, existingID, existingPath)` over the generic `writeErr(w, 409, msg)` so the JSON body carries diagnostic fields the client can use to render a "what should we do?" prompt without an extra resolve call.

Wrap errors with `fmt.Errorf("%w: ...", domain.ErrX)` when adding context — never lose the sentinel via `errors.New`.

## Naming

- Service methods are `Verb+Noun`: `WriteContent`, `ReplaceFile`, `MkdirRelative`.
- Internal helpers are lowercase first letter: `makeUniquePath`, `syncFilePath`.
- Handler functions in HTTP adapter are inline closures inside `NewRouter` — that's intentional, easier to grep by route.

## Field/option additions

When you add a new field to a request body or a new option:

1. Add it to `api/v1/types.go` (the wire contract).
2. Validate it at the HTTP layer; pass a clean typed value into the domain.
3. Pass it through to the domain method's signature — do **not** re-parse strings inside `domain/`.
4. Update the TS SDK at `sdk/ts/src/types.ts` AND the relevant client file (`paths.ts`, `nodes.ts`, `uploads.ts`, ...).
5. Update the Go SDK at `sdk/filegate/<area>.go`.
6. Update the owning pages under `docs-site/docs/en/`, including the deep
   HTTP or TypeScript reference when the public contract changed.

Forgetting any of these has caused user-facing bugs in this repo. There is no automation that catches the omission.

## Comments

Production code is sparse on comments. Add one only when:

- The "why" is non-obvious (e.g., "we hold the lock here because Close races getOrSubmit").
- A future reader could plausibly try to "simplify" the code in a way that re-introduces a known bug.
- A subtle invariant must hold (e.g., "OnConflict is checked at start AND finalize because of race window X").

Don't comment what well-named code already says. Don't write changelogs in comments. Don't reference the current task — that belongs in commit messages.

## Configuration

- Config flows through `cli/config.go` (Viper) → `domain.Config` struct → constructors.
- Defaults are set with `v.SetDefault(...)` AND a fallback inside the unmarshal block (defense in depth).
- Removing a config field? Remove `v.SetDefault` AND struct field AND tests AND docs in the same commit. Leftover defaults are dead code (this has happened before).

## Lifecycle

- The serve command builds a context, starts components, waits on
  signal/error, then shuts down with a 10s deadline.
- Anything that owns goroutines must be stoppable. Two acceptable shapes:
  - **`Close(ctx context.Context) error`** — when shutdown can plausibly
    block on user code or external I/O. The only example today is
    `jobs.Scheduler`. Always pass a context with a deadline at the call
    site.
  - **`Close()` (no context)** — when the wait is bounded by your own
    code. Examples: `uploadSessionManager.Close()` (waits on its own cleanup
    loop) and `closeableHandler.Close() error` (runs the router's
    pre-registered close functions).
- `Scheduler.Close(ctx)` is the canonical pattern for the contextful shape:
  cancels worker context, drains queue, waits for workers up to `ctx`,
  returns `ErrCloseTimeout` if exceeded. Last worker self-signals via an
  atomic counter — no helper goroutine that could itself leak. Replicate
  this pattern for new pool-style components that wait on user code.

## SDK consistency

- Go and TS SDKs expose the same shape: `Paths`, `Nodes`, `Uploads`,
  `Transfers`, `Search`, `Index`, `Stats`.
- Pure helpers (segment planning/checksums, relay) live in dedicated subpackages
  (`sdk/filegate/segments`, `sdk/filegate/relay`) and a dedicated subpath
  export (`@valentinkolb/filegate/utils`) so callers without a token can
  use them.
- Never re-attach pure helpers as fields on the `Filegate` client — that's
  the DRY rule we explicitly removed `Utils`/`UtilsNamespace` for.
- `*Raw` SDK methods (`paths.putRaw`, `nodes.contentRaw`,
  `nodes.thumbnailRaw`, `uploads.sessions.segments.putRaw`) **must** return
  the raw `Response` / `*http.Response` unchanged on 4xx/5xx. They exist
  for relay/passthrough handlers; throwing on non-2xx breaks that
  contract. The Go side has a regression test
  (`TestRawMethodsDoNotThrowOnNon2xx` in `sdk/filegate/client_test.go`).
  The TS side currently has no test harness — only `tsc` build
  verification. Preserve the Go test, and if you ever add a TS test
  setup, port the same property check.
- Conflict diagnostic fields (`existingId`, `existingPath`) MUST be exposed
  as typed fields on the SDK error type, not buried in a stringified body.
  Go: `APIError.ExistingID/ExistingPath` + `IsConflict()`. TS:
  `FilegateError.errorResponse?.existingId/existingPath`.

## What NOT to do

- Don't add interfaces with one implementation "in case we need to mock later". Test seams come from existing layering, not speculative interfaces.
- Don't add a config option for behavior that has no good reason to vary. The codebase has been actively de-configured (e.g., the rename suffix scheme is fixed at `-NN` — no setting).
- Don't add backwards-compatibility shims when nothing depends on the old shape. The project is `v0.0.0`; breaking changes are cheap.
