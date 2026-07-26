# What commit actually costs

Raw data: `tree-bench-20260726_220709-copy-assembly.csv` (copy assembly),
`tree-bench-20260726_221045-move-assembly.csv` (move assembly).
Harness: `bench/scripts/run-tree-bench.sh` with `FILEGATE_BENCH_PRESET=commit`.

Ticket `pvov58oj` asked for two changes and named a cause: commit dominates every
upload-session run "because `assembleUploadSession` reads every staged segment
back and writes the whole file a second time". This measures whether removing
that second write does what the ticket expected.

It does not, for small files. The second write was never the dominant term
there. The number the ticket wanted comes from the other half of the ticket.

## Setup

Same host and harness as `2026-07-26-many-small-files.md`: Apple M1 Max, Docker
Desktop Linux VM, ext4 on a named volume, detector parked, versioning off, stack
recreated between every run, 2 repeats. Both arms ran back to back in one
sitting, because identical configurations on this host differ by up to 2.4x
across tens of minutes.

The two arms differ only in `assembleUploadSession`: the copy arm is the previous
implementation, the move arm renames a lone staged segment into place.

## Assembly got cheaper. Commit did not.

Ranges are min–max across both repeats.

| Configuration | copy: wall (s) | move: wall (s) | copy: commit (s) | move: commit (s) |
|---|---|---|---|---|
| logs, session, 32, batch 100 | 14.4–15.4 | 14.4–14.5 | 366–380 | 378–382 |
| node-modules, session, 32, batch 100 | 32.7–33.2 | 31.1–31.7 | 991–1004 | 939–959 |
| photos, session, 8, 32 MiB segments | 6.6–8.1 | 6.0–6.5 | 34–41 | 30 |
| logs, one-shot PUT, 32 | 5.9–6.6 | 5.9–6.0 | — | — |
| photos, one-shot PUT, 8 | 3.0–3.4 | 3.1–3.2 | — | — |

Commit sums exceed wall time because they add up across 32 workers; the ratio is
the point.

For the two small-file shapes the commit total does not move outside the noise.
For photos — 15 MiB files, still one segment each at a 32 MiB segment size —
commit drops from 34–41 s to 30 s, and throughput goes from 271–334 to 339–364
MiB/s. That is the shape where the copied bytes were actually worth something.

Large-file throughput does not regress: one-shot PUT on photos is 641–724 MiB/s
before and 690–712 after.

## Where commit time goes

`handleCommit` instrumented per phase, 800 single-segment 16 KiB commits per arm,
medians in microseconds. Three move-path runs are shown because the unrelated
phases drift and the drift has to be visible to read the table honestly.

| arm | assemble | hash | parent | replace | state | cleanup | total |
|---|---:|---:|---:|---:|---:|---:|---:|
| copy | 2288 | 165 | 1423 | 3546 | 1417 | 1476 | 11188 |
| move, run 1 | 914 | 208 | 2056 | 4390 | 1637 | 1606 | 11316 |
| move, run 2 | 1155 | 215 | 2328 | 5390 | 2052 | 2012 | 14582 |
| move, run 3 | 1176 | 247 | 2494 | 5724 | 1420 | 2305 | 15644 |

- `assemble` — build the complete file from segments
- `hash` — whole-file MD5 and SHA-256, and the checksum check
- `parent` — `ensureSessionParent`, creating missing directories
- `replace` — `ReplaceFileWithHashes`: publish the bytes, set the xattr, write the index row
- `state` — mark the session committed and write the commit record
- `cleanup` — remove staged segments and the assembled file

Assembly halves: 2288 µs to a 914–1176 µs band that holds across three runs
while `replace` drifts 3546 → 5724 and `parent` drifts 1423 → 2494. The phases
this change does not touch move by 60–75% between runs; the phase it does touch
moves down and stays down. That is as clean a signal as this host gives.

But assembly is 8–20% of a commit. Publishing and recording the file is 40%,
and the three bookkeeping phases — parent, state, cleanup — are another 47%
between them. All of it is fsync and `pebble.Sync` bound. For 16 KiB of payload,
copying the bytes twice was never the expensive part.

A one-shot PUT of the same file costs about 38 ms of server time against about
76 ms for a commit. Commit is roughly a one-shot PUT plus session bookkeeping,
which is what the phase table shows.

## What actually fixed the small-file shapes

The ticket's second change: both SDKs now default `DirectThresholdBytes` to the
segment size, so a file that would have been a single segment takes one PUT
instead of create, segment, commit.

From the table above, on the same host in the same sitting:

| logs, 5000 files | wall (s) | files/s |
|---|---|---|
| session, 32, batch 100 | 14.4–14.5 | 345–348 |
| one-shot PUT, 32 | 5.9–6.0 | 829–843 |

2.4x, and it needs a third of the requests. That is where the small-file win is,
and it is now what callers get without opting in.

## What would make commit itself cheaper

Not more work on assembly. The remaining cost is the number of durable writes per
commit: the session state write and the commit record, the staged-artifact
removal with its two directory syncs, and the parent-directory ensure. Together
those are 47% of a commit and none of them touch file content. Batching or
deferring them is a separate piece of work with its own crash-safety argument to
make, and it is not attempted here.
