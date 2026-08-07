# Module Map

Filegate is a layered Go codebase using the **ports-and-adapters** pattern.
Read this before adding code anywhere — putting code in the wrong layer is
the most common review-blocking mistake.

## Layout

```
cmd/filegate                  → main entrypoint (cobra)
cli/                          → command tree, config loading, lifecycle wiring
adapter/http/                 → HTTP routing, request/response shape, auth, middleware
domain/                       → core business logic + ports (interfaces)
infra/pebble/                 → metadata index implementation (Pebble KV)
infra/filesystem/             → POSIX filesystem + xattr (Linux)
infra/detect/                 → btrfs find-new + poll-based change detection
infra/jobs/                   → bounded worker pool (Scheduler)
infra/eventbus/               → in-memory pub/sub
infra/fgbin/                  → binary record codec for index values
infra/cache/                  → small LRU wrapper
api/v1/                       → JSON request/response types (the wire contract)
sdk/filegate/                 → Go client SDK
sdk/filegate/segments         → pure helpers (segment planning + sha256)
sdk/filegate/relay            → pure HTTP relay helper
sdk/ts/                       → TypeScript client SDK
```

## Import direction — strict

```
cli ──► adapter/http ──► domain ◄── infra/*
                          │
                          └──► (only standard library + a handful of utility
                                infra leaves; no business-logic imports back)
```

The key inversion: **`domain` imports nothing under `github.com/k2b-dev/filegate/v3/*`**.
It defines its own interfaces (`Index`, `Store`, `EventBus` — see
`domain/ports.go`), and `infra/*` packages **import `domain`** to
implement those interfaces. The HTTP adapter wires it all up via
constructors in `cli/serve.go`.

`domain` does import standard library plus a small set of approved
third-party packages (e.g. `bmatcuk/doublestar`, `google/uuid`,
`hashicorp/golang-lru`, `rwcarlsen/goexif`). Adding a new third-party
import in `domain` is OK in principle but raises the bar — discuss before
adding heavy dependencies.

This is the ports-and-adapters / hexagonal architecture pattern. Verify
the no-internal-imports invariant with:

```bash
go list -f '{{ .ImportPath }}: {{ .Imports }}' ./domain | tr ' ' '\n' | grep "k2b-dev/filegate/v3"
# → expect: empty output
```

## What each layer may import (cheat sheet)

| Layer            | May import from                                        |
|------------------|--------------------------------------------------------|
| `cmd/filegate`   | `cli/`                                                 |
| `cli/`           | `adapter/http`, `domain`, all of `infra/*`             |
| `adapter/http`   | `domain`, `api/v1`, `infra/jobs`, `infra/cache`        |
| `domain`         | nothing under `filegate/*`                             |
| `infra/pebble`   | `domain`, `infra/fgbin`                                |
| `infra/filesystem` | `domain`                                             |
| `infra/eventbus` | `domain`                                               |
| `infra/detect`   | (no `domain` import — produces `Event` value type)     |
| `infra/jobs`     | (no `domain` import — generic worker pool)             |
| `infra/cache`    | (no `domain` import — generic LRU wrapper)             |
| `infra/fgbin`    | (no `domain` import — pure binary codec)               |
| `api/v1`         | nothing under `filegate/*`                             |
| `sdk/filegate`   | `api/v1` (type aliases)                                |
| `sdk/filegate/segments`, `sdk/filegate/relay` | nothing under `filegate/*` |

## What must NEVER happen

- `domain` importing `infra/*` or `adapter/*` or `api/v1`.
- `infra/*` importing `adapter/*` or `cli/*`.
- Two infra packages depending on each other in a way that creates a cycle.
- `api/v1` importing anything under `domain` — the wire contract must be a
  pure types package.

If a change requires importing across these boundaries, you are in the wrong
layer. Re-think where the logic belongs.

## Where do I put new code?

| You're adding...                                       | It belongs in                                      |
|--------------------------------------------------------|----------------------------------------------------|
| A new HTTP endpoint                                    | `adapter/http/router.go` + `api/v1/types.go`       |
| Validation/auth/routing logic                          | `adapter/http/`                                    |
| New behavior involving files, ownership, the index     | `domain/service.go` (or sibling like `write_atomic.go`) |
| New error class                                        | `domain/errors.go` + map in `adapter/http/router.go` `statusFromErr` |
| New JSON wire field                                    | `api/v1/types.go` (and TS + Go SDKs)               |
| Pebble key/value layout change                         | `infra/pebble/index.go` + bump `currentIndexFormatVersion` |
| New binary record field                                | `infra/fgbin/record.go` + extension list           |
| New CLI subcommand                                     | `cli/`                                             |
| New upload-session semantics                           | `adapter/http/upload_sessions.go` (handler) + `domain/service.go ReplaceFile` (storage) |
| New SDK helper that doesn't need a connection          | `sdk/filegate/<subpkg>` (e.g. `segments/`, `relay/`) |
| New SDK method that calls Filegate over HTTP           | `sdk/filegate/<area>.go` and `sdk/ts/src/<area>.ts` |

## Big files worth knowing

- [`domain/service.go`](../../../domain/service.go) — ~2400 lines, the
  orchestrator. Use grep, not full reads.
- [`adapter/http/router.go`](../../../adapter/http/router.go) — ~1200 lines,
  all routes + middleware.
- [`adapter/http/upload_sessions.go`](../../../adapter/http/upload_sessions.go)
  — resumable upload-session state machine.
- [`infra/pebble/index.go`](../../../infra/pebble/index.go) — Pebble wrapper
  with format-version guard.
