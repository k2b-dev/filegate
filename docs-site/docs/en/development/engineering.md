---
title: Engineering patterns
navTitle: Engineering patterns
section: Development
order: 330
description: Filegate layering, concurrency, streaming, encoding, testing, and review conventions.
tags: [development, patterns, testing]
---

# Code Patterns

## Principles

- Reliability over peak speed
- Explicit limits on queues/body sizes
- Deterministic behavior under retries and duplicates
- Small interfaces between modules

## Patterns Used in This Repo

### 1. Adapter/Domain/Infra split

- `adapter/http` handles protocol concerns only
- `domain` owns behavior/invariants
- `infra/*` owns storage and external system details

### 2. Index-first metadata reads

Metadata reads should not stat filesystem on hot path.

- read from Pebble
- use targeted refresh only when required

### 3. Bounded async work

All expensive async work runs through bounded queues and worker pools.

Do not add unbounded goroutine fan-out for image/exif/background jobs.

### 4. Explicit recursive behavior

Recursive ownership updates are controlled with explicit flags/parameters.

### 5. Stream, do not buffer

For content endpoints and relay helpers:

- use streams (`io.CopyBuffer`)
- avoid loading full files in memory

### 6. Canonical binary encoding

`infra/fgbin` enforces strict decode invariants (version/type/order/length checks).

## Testing Expectations

- Unit tests for pure logic
- Integration tests for filesystem-sensitive flows
- Fuzz tests for binary/parser/upload boundary conditions
- Benchmark tests for index hot path

## Review Checklist (Quick)

- Are path/root confinement guarantees preserved?
- Are new APIs reflected in `api/v1` types and docs?
- Are write paths crash-safe and idempotent where needed?
- Are limits/defaults sane for production?
- Are tests and benchmarks updated?
