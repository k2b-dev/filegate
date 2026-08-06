# Testing

## What to run when

| You changed...                        | Minimum verification                                         |
|---------------------------------------|--------------------------------------------------------------|
| Pure logic, no I/O                    | `go test ./<changed-pkg>`                                    |
| Anything with goroutines              | `go test -race -count=20 ./<changed-pkg>` (catch flakes)     |
| Anything Linux-specific (`*_linux*.go`)| Docker — see "Running Linux tests" below                    |
| Upload session segment/commit paths   | Both Docker tests AND `make fuzz-smoke`                      |
| The Pebble index format               | `make fuzz-smoke` (fgbin codec) + Docker rescan tests        |
| Adapter/HTTP                          | Docker (almost all HTTP tests are Linux-tagged)              |
| Runtime config or container layout    | `make test-docker-runtime-state`                              |
| Detector/versioning capability logic  | Real btrfs Docker targets below                               |
| Admin app                             | Frozen install, audit, typecheck, tests, build, container     |

## Running Linux tests on macOS dev

The repo has many `*_linux*.go` test files (build-tag protected). They don't compile on macOS. Use:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.25 sh -c "go test ./..."
docker run --rm -v "$PWD":/src -w /src golang:1.25 sh -c "go test -count=2 -race ./..."
```

`make test-race` runs the full Linux suite twice under the race detector in
Docker. Two iterations with race is the standard "it's really green" check.
Bump to `-count=3` or higher when stress-hunting an intermittent flake.

Some tests are explicitly skipped without env vars:

- `FILEGATE_SOAK=1` — long detector soak test
- `FILEGATE_CHAOS=1` — chaos tester for the change detector
- `FILEGATE_BTRFS_REAL=1` (+ `FILEGATE_BTRFS_REAL_ROOT`) — real btrfs subvolume tests

Don't enable these in routine work — they take minutes.

The real-btrfs suite also contains one explicit known-limitation skip:
externally deleting a nested subvolume inside a watched root can stall the
parent detector until restart. Do not report that skip as tested support; it is
an operator constraint documented in `docs/sysadmin.md`.

CI uses loopback btrfs images for the deterministic capability gates:

```bash
make test-detector-btrfs-real-docker
make test-versioning-btrfs-real-docker
```

The container recreation contract is checked with:

```bash
make test-docker-runtime-state
```

## Test naming + organization

- One concept per test function. `TestX_Default`, `TestX_WithY`, `TestX_RejectsZ`.
- Linux-only tests: filename ends `_linux_test.go` AND first line is `//go:build linux`.
- `t.Parallel()` is **only** for tests that share no mutable state with
  other tests in the same package. The current `domain`, `infra/jobs`, and
  `infra/pebble` race/concurrency tests deliberately do NOT use it because
  they share temp dirs, exercise the same scheduler/pebble singletons, or
  rely on deterministic timing. The SDK client unit tests do use it
  because they spin their own `httptest.Server` per test.
- Use `t.TempDir()` for any filesystem state — auto-cleanup, parallel-safe.

## Coverage gaps to fill when you add features

When you add a new endpoint or a new write surface, the table-stakes tests are:

1. **Happy path** — works without options, returns 200/201.
2. **Default conflict mode** — gets `error` (this is now uniform; assert it).
3. **Each non-default conflict mode** (`overwrite`, `rename`, `skip` if applicable).
4. **Cross-type conflict** — file-where-dir-expected, dir-where-file-expected.
5. **Adversarial inputs** — empty body, oversized body, path traversal, non-UTF-8, control characters.
6. **Concurrency** — at minimum a `-race` run; for state-machine code, an explicit race test (see `TestRescanRaceWithConcurrentAPIMutations` for the template).

Look at `adapter/http/router_conflict_linux_test.go` for the conflict-test pattern — copy it for new endpoints.

## Channel-based sync, not Sleep

This is repeated in [`concurrency.md`](concurrency.md) but it's worth saying twice: tests in this repo do not use `time.Sleep` to synchronize between goroutines. If you find yourself writing `time.Sleep(50 * time.Millisecond)` and then asserting state, stop. Use:

- An internal observable counter (`joiners` on `jobCall`/`dirSyncFlight` for coalescers).
- A barrier pattern: `var ready sync.WaitGroup; ready.Add(N); ... ; ready.Wait()`.
- An explicit `started` channel closed by the inner function.

CI on overloaded runners has caught time-sleep-based tests as flakes; the conversion to channel-based sync was a substantial cleanup. Do not regress.

## Fuzz smoke

`make fuzz-smoke` runs fgbin fuzz targets for 10 seconds each:

- `infra/fgbin.FuzzDecodeEntity`
- `infra/fgbin.FuzzDecodeChild`

Run before opening a PR that touches the codec. New crash inputs land in
`testdata/fuzz/<func>/` — commit them.

## Commitable test artifacts

- `testdata/` — checked-in test fixtures (small).
- `testdata/fuzz/<FuzzFunc>/` — auto-discovered fuzz crashers; check these in.

Do NOT check in:

- Pebble index databases (binary, large, churny).
- `.fg-uploads/` staging from running the daemon.
