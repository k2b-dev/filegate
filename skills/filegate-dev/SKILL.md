---
name: filegate-dev
description: Work effectively on the Filegate Go codebase itself — implementing features, fixing bugs, refactoring, writing tests. Use whenever the task touches Filegate's own production code, internal packages (domain, infra/*, adapter/http, cli, sdk/filegate), or the surrounding test/build/release infrastructure. Triggers include "filegate bug", "add a feature to filegate", "refactor the filegate index", "write a test for upload sessions", "review my filegate change". This is for working ON Filegate, not for using it from another project — for that, use the `filegate` skill instead.
---

# Filegate Dev

You are working on the Filegate codebase itself: a Linux-only HTTP gateway over the local filesystem with a Pebble-backed metadata index, btrfs/poll change detection, resumable upload sessions, thumbnails, EXIF, and a stable file-id model.

Filegate is treated as production infrastructure: durability and security come before clever throughput tricks, write paths must be idempotent, and every behavior change ships with a test that would have caught the previous behavior.

## Workflow

1. **Locate the right layer.** Filegate is a layered architecture (`adapter/http` → `domain` → `infra/*`). Read [`references/module-map.md`](references/module-map.md) before editing — putting code in the wrong layer is the most common review-blocking mistake.
2. **Read the relevant convention reference.** Pull only the file you need:
   - Conflict handling on writes? → [`references/conflict-handling.md`](references/conflict-handling.md)
   - Code style, layering, error model? → [`references/conventions.md`](references/conventions.md)
   - Concurrency and resource lifecycle? → [`references/concurrency.md`](references/concurrency.md)
3. **Plan the change with tests in mind.** [`references/testing.md`](references/testing.md) lists the patterns this repo uses, including the channel-based synchronization rule (no `time.Sleep` for sync between goroutines, ever).
4. **Implement.** Surgical edits, no drive-by refactors of unrelated code, no speculative abstractions.
5. **Run the verification gates.** [`references/verification.md`](references/verification.md) is the canonical list — `go vet`, `staticcheck`, race tests, Linux Docker tests for `_linux_test.go` files, fuzz smoke. Skipping any of these has historically allowed real bugs through.
6. **Update docs.** If `api/v1/types.go`, the HTTP routes, the CLI surface, or any public SDK changed, update the owning Fibel pages under `docs-site/docs/en/` in the same change. The repo's own check-list ([`references/verification.md`](references/verification.md)) calls this out.

## Hard Rules — non-negotiable

These come from past incidents in this codebase, not abstract style preferences:

- **Never silently overwrite or drop data on a name collision.** Every
  write surface that can hit a name collision (`PUT /v1/paths`,
  `POST /v1/nodes/{id}/mkdir`, `POST /v1/uploads/sessions`,
  `POST /v1/transfers`) defaults to `onConflict: "error"`. `PUT
  /v1/nodes/{id}` is exempt — it addresses an existing node by ID and is
  defined as content-replacement. Adding a new write surface? If it can
  hit a name collision, it must follow the same `onConflict` scheme —
  see [`references/conflict-handling.md`](references/conflict-handling.md).
- **Production content writes go through the blessed helpers — never direct
  `os.WriteFile`/`OpenFile`.** The blessed paths preserve the xattr file
  ID, do symlink-rejection, and most do temp-file+atomic-rename:
  - `writeFileAtomic` (in `domain/write_atomic.go`) — full
    temp-file+rename with parent fsync. Used by `createAndWriteContent`
    and `WriteContent`.
  - `ReplaceFile` (upload session commit) — relies on Linux `rename(2)`
    atomic-replace as the fast path, with a non-atomic `OpenWrite +
    truncate + io.Copy` fallback for cross-device cases. The fast path
    is atomic; the fallback is not. If you change `ReplaceFile`, keep
    the fast path on top.
  - `Transfer copy` and `copyPath` — non-atomic `OpenWrite + io.Copy`.
    Used for cross-mount or directory-tree copies. Acceptable because
    Transfer is a high-level operation already; just don't claim
    atomicity for these paths.
  Bypassing all of these (raw `os.WriteFile`) loses the xattr ID and the
  stable-ID invariant goes with it.
- **Bypass root/path confinement at your peril.** Filegate accepts four
  distinct path-input shapes; each has its own validator. Use the right
  one:
  - **Virtual path** (`mount-name/...`, e.g. `PUT /v1/paths`): goes through
    `sanitizeVirtualPath` → mount resolution → `safeResolvedPath`.
  - **Relative path** (`mkdir` body's `path`): goes through
    `sanitizeRelativePath`, then walked under the parent's resolved abs.
  - **Single filename** (transfer `targetName`):
    must reject empty and any `/`. The handlers do this inline.
  - **Node ID** (most `/v1/nodes/{id}` routes): resolved via
    `ResolveAbsPath` which is index-backed — no string-level path math.
  Adding a new path-accepting endpoint? Match the input shape to one of
  the existing validators; never roll your own. Re-read
  [`adapter/http/router_security_linux_test.go`](../../adapter/http/router_security_linux_test.go).
- **Index writes must be batched.** All index mutations go through
  `idx.Batch(func(b Batch) error { ... })`. Never call `b.PutEntity`
  outside a batch — partial writes leave the index inconsistent.
- **Index changes need cache invalidation AND event publishing — both
  centralised in the helpers.** Use the blessed sync/delete helpers and
  they handle both for you:
  - `syncSingle(absPath)` — invalidates cache, publishes one
    `EventUpdated{ID, Path=absPath}`.
  - `syncSubtree(absPath)` — batches per-descendant index updates,
    invalidates cache, publishes one **bulk** `EventUpdated{ID,
    Path=absPath}` for the subtree root (no per-descendant events).
  - `deleteSubtree(rootID)` — batches per-descendant index deletes,
    invalidates cache, publishes one **bulk** `EventDeleted{ID,
    Path=root}`. Callers like `Delete`/`RemoveAbsPath` rely on this and
    must NOT publish a redundant `EventDeleted` themselves.
  - Public mutators emit semantic events where they can distinguish the
    operation: new entities emit `EventCreated`, content/metadata changes
    emit `EventUpdated`, renames and re-parents emit `EventMoved`, and
    deletes emit `EventDeleted`. Keep those emissions single-shot; the
    Linux event tests pin the no-duplicate contract.
  Forgetting cache invalidation silently desyncs the cache from the
  index; using the helpers gets both right by construction.
- **Bounded async work is mandatory; the implementation can vary.** No
  unbounded `go func() { ... }()`. Three patterns are blessed in this
  repo, in order of preference: (1) `infra/jobs.Scheduler` for keyed,
  deduplicated background work; (2) `infra/eventbus`'s bounded async
  dispatch with parallelism semaphore; (3) component-local cleanup
  goroutines bounded by their own stop channel + a write semaphore
  (see `uploadSessionManager`). All three must be stoppable via the
  constructor's context or an explicit `Close(...)`.
- **`Scheduler.Close(ctx)` must respect the context deadline** even when
  jobs ignore cancellation — that's why it uses an atomic worker counter
  rather than `wg.Wait()` (which could pin a leaked helper goroutine).
  Preserve that property.
- **Tests do not use `time.Sleep` to synchronize goroutines.** This
  codebase explicitly fixed several flaky tests by replacing sleeps with
  channels, atomic counters on internal state (`joiners` on `jobCall` /
  `dirSyncFlight`), or barrier-pattern waits. See
  [`references/testing.md`](references/testing.md).
- **Sleep is allowed for** race-window simulation, rate limiting, polling
  helpers waiting on eventually-consistent FS state (e.g. `waitUntil` in
  the detector tests), or when the test is genuinely waiting on real
  external state with no observable signal. **Never** for "wait until the
  other goroutine has progressed past line N".
- **Linux-only paths must have a `//go:build linux` tag and live in
  `*_linux*.go` files.** macOS dev environments cannot compile them; CI
  does, via Docker.
- **Add a regression test for every bug fix.** "It works on my machine
  now" is not enough — encode the failure mode.
- **Dead code goes.** If a function/field/option becomes unreachable after
  your change, delete it in the same commit. `staticcheck` and `go vet`
  are part of the verification gate.

## Common pitfalls — read [`references/conventions.md`](references/conventions.md) before editing if any apply

- Adding HTTP fields without updating `api/v1/types.go` AND the TS SDK in
  `sdk/ts/src/types.ts` AND the Go SDK in `sdk/filegate/` AND the
  corresponding Fibel pages under `docs-site/docs/en/`.
- Calling `s.svc.X()` from inside a long-held lock — leads to lock-ordering
  deadlocks.
- Mishandling `os.ErrNotExist` in rescan walks. Two classes:
  (a) **harmless live-race ENOENT** — a file vanished between WalkDir and
      our subsequent stat/setxattr; skip and continue (current rescan code
      already does this, on purpose).
  (b) **stale-index ENOENT** — the indexed path no longer exists on disk;
      the detector's `SyncAbsPath`/`RemoveAbsPath` flow must clean it up.
  Don't conflate them: silently swallowing class (b) leaves zombie
  entries forever; treating class (a) as fatal aborts the whole rescan.
- Reusing the same `time.Sleep`-based synchronization that has bitten the
  repo before.

## When in doubt

If the change touches Pebble layout, the index format version, or upload-session metadata schema: stop and discuss before coding. These have versioning concerns and rebuild-on-incompatible-format paths that must be updated in lockstep.
