---
title: Benchmarking Filegate
navTitle: Benchmarks
section: Development
order: 340
description: Reproduce Filegate microbenchmarks, HTTP load tests, and tree-upload measurements.
tags: [development, benchmarks, performance]
---

# Benchmarks

This repository provides reproducible benchmark entry points for:

- microbenchmarks (Go test benchmark framework)
- HTTP end-to-end load benchmarks
- compose-based benchmark runs

## Prerequisites

- Go (as defined by `go.mod` toolchain)
- Docker + Docker Compose (for compose benchmark profile)
- curl

## Commands

From repo root:

```bash
make test-detector-linux
make test-detector-soak
make test-detector-chaos
make test-detector-btrfs-real   # requires real btrfs host path
make bench-go
make bench-http      # requires a running filegate endpoint
make bench-compose   # starts compose.test.yml automatically
```

`make test-detector-linux` runs linux-only detector/index sync tests (poll/ext4-like path, btrfs-like unknown fallback path, and duplicate-event stress).
`make test-detector-soak` runs long-running external-change sync validation.
`make test-detector-chaos` runs burst/overlap stress (duplicates + frequent unknown/rescan).
`make test-detector-btrfs-real` runs a real btrfs end-to-end detector/index sync test on host.

## Go Microbenchmarks

Script: [`bench/scripts/run-go-benches.sh`](https://github.com/ValentinKolb/filegate/blob/main/bench/scripts/run-go-benches.sh)

Covers:

- `infra/fgbin` codec benchmarks
- `infra/pebble` hot metadata lookup benchmarks

Output files are written to `bench/results/`.

## HTTP Load Benchmarks

Tooling:

- Go load generator: [`cmd/filegate-bench/main.go`](https://github.com/ValentinKolb/filegate/blob/main/cmd/filegate-bench/main.go)
- matrix script: [`bench/scripts/run-http-bench.sh`](https://github.com/ValentinKolb/filegate/blob/main/bench/scripts/run-http-bench.sh)

Default matrix scenarios:

- `metadata-path`
- `metadata-id`
- `read-4k`
- `read-1m`
- `write-4k`
- `write-1m`
- `mixed`

Metrics per run:

- `ops/s`
- `mbit/s`
- error rate
- avg/p50/p95/p99 latency

Environment variables used by scripts:

- `FILEGATE_BASE_URL`
- `FILEGATE_TOKEN`
- `FILEGATE_PATH_BASE`
- `FILEGATE_BENCH_DURATION`
- `FILEGATE_CLIENT_MATRIX` (e.g. `\"1 8 32 128\"`)

## Compose Profile

Script: [`bench/scripts/run-http-bench-compose.sh`](https://github.com/ValentinKolb/filegate/blob/main/bench/scripts/run-http-bench-compose.sh)

This script:

1. Builds and starts `compose.test.yml`
2. Waits for `/health`
3. Runs benchmark matrix
4. Stops compose stack

## Many-Small-Files Upload Benchmark

Script: [`bench/scripts/run-tree-bench.sh`](https://github.com/ValentinKolb/filegate/blob/main/bench/scripts/run-tree-bench.sh)

The load-generator scenarios above measure steady-state ops/s for one request
shape. Folder uploads need a different question answered — how long a whole
corpus takes and which lever moves that — so `cmd/filegate-bench --mode tree`
uploads a fixed corpus to completion and reports wall time, files/s, MiB/s and a
per-phase split (hash, session create, segment PUT, commit).

Corpus shapes (`--tree-shape`, scaled with `--tree-scale`):

- `photos`: 600 mostly-multi-megabyte files with a large-file tail
- `logs`: 20000 files of 1–32 KiB
- `node-modules`: 20000 mostly-tiny files in a deep tree

Upload paths (`--tree-transport`): `put` (one-shot `PUT /v1/paths`), `direct`
(mint a signed URL, then PUT), `session` (hash, create, segment PUT, commit).

Levers: `--tree-files`, `--tree-segments`, `--tree-hash`, `--tree-create`,
`--tree-segment-size`, `--tree-batch`, `--keep-alive`, `--http2`.

The script runs its own stack (`bench/compose.bench.yml`, ports 4911 and 4913,
its own volumes) including a TLS edge, so browser-facing HTTP/2 can be measured
the way it is actually deployed. The listener itself also speaks h2c when
`server.http2_cleartext` is on, which the `h2c` preset uses to compare protocols
on one port with no proxy in between. It recreates the stack between
configurations, since a corpus left behind changes what later runs measure.

Presets (`FILEGATE_BENCH_PRESET`): `full` (everything, the default), `levers`,
`http2`, `shapes`, `commit`, and `h2c`. Use `commit` for before/after work on the
commit path — it is the narrow set of session runs plus a large-file regression
check, small enough to run once per build in a single sitting, which matters
because this comparison is only meaningful when both arms ran back to back.
Use `h2c` to compare HTTP/1.1 against cleartext HTTP/2 on the listener itself;
both arms run against the same server, so it needs no rebuild between them.

Committed results and interpretation:

- [What upload commit costs](/docs/en/development/benchmarks/results/commit-cost)
- [Reducing durable writes during commit](/docs/en/development/benchmarks/results/commit-durable-writes)
- [Many small files](/docs/en/development/benchmarks/results/many-small-files)
- [HTTP/1.1 compared with h2c](/docs/en/development/benchmarks/results/h2c)

## Notes

- `metadata-*` scenarios benchmark API metadata routes, not content download.
- For fair before/after comparisons, keep:
  - same host
  - same filesystem
  - same client matrix
  - same duration and payload sizes
- The detector stress pipeline is defined in `.github/workflows/detector-soak.yml`.
