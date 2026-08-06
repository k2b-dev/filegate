---
title: Reducing durable writes during commit
navTitle: Durable writes
section: Benchmark results
order: 420
description: Evidence behind the upload commit durable-write changes.
tags: [benchmarks, uploads, durability]
---

# Cutting durable writes out of upload commit

Follow-up to `2026-07-26-commit-cost.md`, which established that commit is fsync
bound rather than copy bound, and that the bookkeeping around publishing a file
costs more than publishing it.

## What the evidence is, and why it is not wall time

The claim here is about a count, so it is measured as a count.

`domain/service_durable_writes_linux_test.go` wraps the index with a decorator
that counts write batches. Every batch commits with `pebble.Sync`, so the count
is the number of durable writes on the path. It is deterministic and independent
of machine load, which matters because the host was saturated while this work
landed — load average 34 on ten cores, thirty unrelated containers running — and
wall-clock arms taken in that window disagreed with each other by 3x in both
directions. Numbers that cannot be reproduced are not reported here.

| Operation | Before | After |
|---|---:|---:|
| Create a 1-level directory | 1 | 1 |
| Create a 2-level chain | 2 | 1 |
| Create a 5-level chain | 5 | 1 |
| Create an 8-level chain | 8 | 1 |
| Re-enter an existing 3-level chain | 0 | 0 |
| Write a file at depth 2 | 4 | 3 |
| Write a file at depth 8 | 10 | 3 |

The last two lines are the shape that matters: before, the cost of writing a file
grew with the depth of the tree it landed in. It no longer does.

Phase timings taken before the host degraded, normalized against `replace` — an
untouched phase in the same run, so machine drift cancels:

| Phase, as a multiple of `replace` | logs before | logs after | node-modules before | node-modules after |
|---|---:|---:|---:|---:|
| `parent` | 0.027 | 0.007 | 2.439 | 1.011 |
| `cleanup` | 0.407 | 0.005 | 0.305 | 0.004 |
| `assemble`, `hash`, `marker`, `state` | — | unchanged | — | unchanged |

Read as "how many publish-steps does a commit cost", a node-modules commit went
from 4.6 to 2.8.

## The three changes

### The parent directory is resolved, not re-created

`ensureSessionParent` walked the path and called `MkdirRelative` for every level
on every commit, whether or not the directory existed. Each of those calls took a
path lock, walked its own prefix and read the index, so the work scaled with tree
depth and was paid once per file.

It now resolves the parent first and returns if that succeeds, which is what
almost every commit after the first file in a directory does. Nothing is created
on that path, so the rollback closure stays the no-op it should be.

### A directory chain is indexed in one write

Creating a chain still has to create it, and that was the remaining cost: one
synced index write per level, because `syncSingle` indexes one path per call and
reaches ancestors by recursing upward.

`indexNewDirChain` writes the whole run in one batch. The levels were created
together and are only reachable through each other, so a single atomic write is
both cheaper and a stronger guarantee than a chain another reader can observe
half-indexed.

**Crash safety.** A crash before the batch commits leaves the directories on disk
and absent from the index — the state the index is already built to recover from,
since it is rebuildable from the filesystem. A crash after leaves them present and
indexed. There is no in-between, because the batch is atomic. This is strictly
better than the per-level path, where a crash could leave a chain indexed halfway.

**The bug this introduced, and the invariant that caught it.** The caller reports
the levels it created, and under concurrency that list can skip a middle level:
another request creates it between this one's lstat and its mkdir. Chaining across
that gap anchored a directory to its grandparent and silently dropped a path
component, so writes landed at the wrong place instead of failing. The
shared-parent regression test from `0hj2gkrk` caught it on the first run.
`indexNewDirChain` now keeps only the trailing contiguous run and lets the parent
resolution index the rest, and
`TestDirectoriesResolveWhenAnIntermediateLevelIsCreatedConcurrently` pins it.

### Staged artifacts are derived, not scanned, and not fsynced

`removeSessionArtifacts` globbed the stage directory for orphaned segments. That
directory is shared by every session on the mount, so a five-thousand-file upload
performed five thousand scans of a five-thousand-entry directory. Segment paths
are deterministic — index `i` in `[0, TotalSegments)`, validated by the PUT
handler, and partial writes carry a `.upload-segment-*` name the glob never
matched anyway — so they are derived instead. That also drops an index read.

The two directory fsyncs are gone.

**Crash safety.** The only thing those fsyncs guaranteed was that the deletion of
temporary staging files survives a crash. A resurrected staging file is harmless:
a committed session answers from its commit record without consulting segments, an
aborted one is closed to writes, and the cleanup loop already sweeps committed and
aborted sessions. Nothing reads those paths expecting them absent.

## What was left alone, and why

Two synced writes remain on the commit path and both earn their fsync.

The **committing marker** written before publishing looks removable — it is a
whole durable write to record an intention. It is what makes a crash between
publishing the bytes and writing the commit record recoverable: on retry the
phase says `committing`, and the recovery path checks whether the file landed.
Without it the retry re-publishes, and under `onConflict=error` a successful
upload turns into a permanent conflict.

The **commit record** is what makes commit idempotent. It is already batched with
the session update, so the two are one write, not two — the original ticket
description was wrong about that.

The directory fsync in `writeSegmentFile` also stays. Dropping it would make a
segment's existence non-durable, and unlike staging garbage that is load-bearing:
the index would claim a segment is uploaded while the file is gone, and a client
that has finished uploading would get a failing commit rather than a recoverable
one. Resumable upload is the entire point of the session path.
