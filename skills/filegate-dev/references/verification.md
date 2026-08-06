# Verification Gates

The minimum bar before you say "done". In order; stop at the first failure and fix.

## 1. Build clean

```bash
go build ./...
```

No errors, no warnings (the macOS LC_DYSYMTAB linker warning when running with `-race` is benign and known — ignore it).

## 2. `go vet`

```bash
go vet ./...
```

Must be silent.

## 3. `staticcheck`

```bash
out=$($(go env GOPATH)/bin/staticcheck ./... 2>&1)
echo "$out" | grep -q "matched no packages" && { echo "STATICCHECK SAW NO PACKAGES — wrong cwd?"; exit 1; }
[ -n "$out" ] && { echo "$out"; exit 1; }
echo "staticcheck: clean"
```

Install once: `go install honnef.co/go/tools/cmd/staticcheck@latest`.

**Critical guard**: `staticcheck` exits 0 even when it prints
`warning: "./..." matched no packages` (e.g. when run from a directory
with no Go files, or when the module isn't selected). A bare `staticcheck
./...` with empty output looks identical to "all clean" — but means
"didn't lint anything". Always grep for the warning string OR confirm at
least one expected check ran. The snippet above does both.

## 4. Dead code

```bash
$(go env GOPATH)/bin/deadcode -test ./...
```

Install once: `go install golang.org/x/tools/cmd/deadcode@latest`. Acceptable false positives in this repo:

- SDK methods (`sdk/filegate/...`) — public library API used by external consumers.
- Some functions guarded by `//go:build linux` aren't seen on macOS.

Anything else: delete it in the same commit, or justify why it must stay.

## 5. Vulnerability scan

```bash
go install golang.org/x/vuln/cmd/govulncheck@latest
$(go env GOPATH)/bin/govulncheck ./...
```

Reachable vulnerabilities fail the gate. Also run dependency audits in the TS
workspaces below.

## 6. Tests on macOS

```bash
go test -count=1 ./...
```

Catches non-Linux-tagged regressions. Fast.

## 7. Race tests

```bash
go test -count=20 -race ./<changed-pkg>
```

For anything with goroutines, run the changed package 20× under `-race`. If your change touches a shared package (jobs, pebble, domain), include those packages too.

Before merge, run the full Linux race suite:

```bash
make test-race
```

## 8. Linux Docker tests

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.25 sh -c "go test ./..."
```

The bulk of HTTP, domain, and CLI tests are Linux-tagged because they exercise xattr, btrfs, real-FS atomic operations. They MUST pass before merge.

For higher-confidence runs (pre-PR or after touching shared infra):

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.25 sh -c "go test -count=2 -race ./..."
```

## 9. Fuzz smoke

```bash
make fuzz-smoke
```

10 seconds per fuzz target × 2 targets. Required if your change touched:

- `infra/fgbin/` (record codec)
- `adapter/http/upload_sessions.go` (segment hashing/writing)
- Anything else with explicit fuzz coverage

If a target finds a new crasher, the input is saved under `testdata/fuzz/<FuncName>/` — commit it and fix the underlying bug.

## 10. TS SDK (when SDK changed)

From the repo root:

```bash
(cd sdk/ts && npm ci && npm audit --audit-level=high && npm test && rm -rf dist && npm run build)
```

Must finish with no `tsc` errors. If you added new fields/methods, also run
a quick smoke import to confirm the published shape — from the repo root:

```bash
node -e 'import("./sdk/ts/dist/index.js").then(m => console.log(Object.keys(m)))'
```

(If you `cd sdk/ts` first, the import path becomes `./dist/index.js`. Pick
one and stick with it; the example above stays in the repo root.)

## 11. Admin app (when Admin or shared contracts changed)

```bash
(cd admin && bun install --frozen-lockfile && bun audit && bun run typecheck && bun test && bun run build)
docker build --file admin/Dockerfile .
```

The Docker build must use the lockfile; do not add a fallback unfrozen install.

## 12. Container state (when config, credentials, or Compose changed)

```bash
make test-docker-runtime-state
```

This proves the manifest, generated REST token, S3 key, and index survive a
container recreation when the documented volumes are retained.

## 13. Real filesystem capabilities (when detector/versioning changed)

```bash
make test-detector-btrfs-real-docker
make test-versioning-btrfs-real-docker
```

These loopback tests exercise real btrfs generation and `FICLONE` behavior.
Skipped host-only tests are not equivalent evidence.

## 14. Workflow syntax (when CI/release YAML changed)

```bash
actionlint
```

Release verification must run from the tagged commit, prove that the tag's SHA
is on `main`, and complete the same security, race, Admin, SDK, Docker-state,
and real-btrfs gates before publishing.

## 15. Docs sync (when API changed)

If you changed any of:

- `api/v1/types.go` (wire contract)
- `adapter/http/router.go` route handlers (added/removed/renamed an endpoint)
- `cli/` command surface
- The TS or Go SDK public surface

Then update the corresponding docs in the same commit:

- `docs-site/docs/en/reference/http-routes.md` and
  `docs-site/docs/en/reference/http-types.md` — for HTTP API
- `docs-site/docs/en/ts-sdk.md` and
  `docs-site/docs/en/reference/typescript-client.md` — for TS SDK changes
- `docs-site/docs/en/reference/cli.md` — for CLI changes
- `README.md` — for top-level user-facing changes

Reviewers look at this. PRs without doc updates for behavior changes get bounced.

## The pre-PR core check

```bash
go vet ./... && \
  $(go env GOPATH)/bin/staticcheck ./... && \
  $(go env GOPATH)/bin/deadcode -test ./... && \
  $(go env GOPATH)/bin/govulncheck ./... && \
  make test-race && \
  make fuzz-smoke && \
  echo "ready for review"
```

Add the scoped Admin, SDK, Docker-state, real-btrfs, and workflow gates above
when the changed paths require them. CI runs the complete production gate set.
