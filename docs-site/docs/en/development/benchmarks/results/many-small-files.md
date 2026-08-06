---
title: Many small files
navTitle: Many small files
section: Benchmark results
order: 430
description: Measurements and conclusions for tree uploads with many small files.
tags: [benchmarks, uploads, performance]
---

# Many small files

Raw data: `tree-bench-20260726_192834.csv` (every row, both repeats).
Harness: `bench/scripts/run-tree-bench.sh`, `cmd/filegate-bench --mode tree`.

This benchmark measures folder-upload throughput, identifies the effective
tuning levers, and evaluates packed segments for small-file workloads.

## Setup

| | |
|---|---|
| Host | Apple M1 Max, 10 cores, 64 GiB, macOS 26.5.2 |
| Runtime | Docker Desktop 29.6.2, Linux VM with 10 CPUs / 23.4 GiB |
| Server | this repo's `Dockerfile`, `bench/compose.bench.yml`, ext4 on a Docker named volume |
| Client | the same load generator in a sibling container on the same Docker network |
| Detector | poll interval parked at 24h |
| Versioning | disabled (no btrfs in the VM) |
| Repeats | 2 per configuration, stack recreated between every run |

The client runs in a container rather than on the host so the measurement is not
dominated by Docker Desktop's port forwarding. The detector is parked because
its rescan walks the whole tree on every tick, and on a corpus of tens of
thousands of files that scan lands inside the measured window. Detector cost is
a real cost, but it is a different question from upload throughput — see the bug
note at the end, where the detector matters for a different reason.

### Scale, and where it was reduced

- `photos`: run at both 150 files / 2.19 GiB and 600 files / 8.49 GiB (9.11 GB).
- `logs`: 5000 files / 81 MiB. Per-file
  overhead is what this shape measures and it does not change with file count.
- `node-modules`: 5000 files / 44 MiB.

### Honesty about noise

Identical configurations differ by up to 2.4x on this host depending on when
they ran; the Docker VM's disk and page cache drift over tens of minutes. Two
repeats plus a discarded warm-up run keep that visible rather than hidden, and
every claim below rests on a gap much larger than the spread. Where a gap is
inside the noise, it is reported as "no measurable difference" rather than
rounded into a result.

## Results

Ranges are min–max across both repeats.

### logs — 5000 files, 81 MiB, ~16 KiB average

| Configuration | wall (s) | files/s | HTTP requests | errors |
|---|---|---|---|---|
| one-shot PUT, 1 in flight | 28.4–32.3 | 155–176 | 5000 | 0 |
| one-shot PUT, 8 | 8.8–9.2 | 544–571 | 5000 | 0 |
| one-shot PUT, 32 | 6.3–7.9 | 629–796 | 5000 | 1 |
| one-shot PUT, 64 | 6.1–6.4 | 780–815 | 5000 | 3–33 |
| one-shot PUT, 128 | 6.7–9.2 | 546–742 | 5000 | 22–95 |
| one-shot PUT, 8, keep-alive off | 12.4–13.9 | 359–403 | 5000 | 0 |
| one-shot PUT, 32, keep-alive off | 7.2–7.9 | 633–693 | 5000 | 9–21 |
| signed direct URL + PUT, 32 | 7.3 | 683–686 | 10000 | 0 |
| session, 32, one create per file | 24.3–58.7 | 85–205 | 15000 | 0 |
| session, 32, batch of 32 | 17.0–19.5 | 257–294 | 10157 | 0 |
| session, 32, batch of 100 | 22.1–24.2 | 207–226 | 10050 | 0 |
| session, 64, batch of 100 | 22.6–25.2 | 198–221 | 10050 | 0 |

Batched session creation was re-run three times back to back, alternating, to
take host drift out of the comparison:

| | run 1 | run 2 | run 3 | median |
|---|---|---|---|---|
| one create per file | 334 files/s | 259 | 142 | 259 |
| batch of 100 | 302 files/s | 285 | 142 | 285 |

### node-modules — 5000 files, 44 MiB, deep tree

| Configuration | wall (s) | files/s |
|---|---|---|
| one-shot PUT, 32 | 23.8–28.2 | 177–210 |
| session, 32, batch of 100 | 41.3–44.0 | 114–121 |

### photos

| Configuration | files | size | wall (s) | MiB/s |
|---|---|---|---|---|
| one-shot PUT, 8 | 150 | 2.19 GiB | 5.9–7.6 | 288–369 |
| session, 8, 8 MiB segments | 150 | 2.19 GiB | 10.6–10.9 | 201–207 |
| session, 8, 32 MiB segments | 150 | 2.19 GiB | 10.2–10.3 | 213–214 |
| one-shot PUT, 8 (full size) | 600 | 8.49 GiB | 24.2 | 359 |
| session, 8, 32 MiB segments (full size) | 600 | 8.49 GiB | 38.5 | 226 |

### HTTP/1.1 vs HTTP/2, both through the same TLS edge

At the time of this measurement, the protocol comparison ran through a Caddy
reverse proxy that terminated TLS and negotiated by ALPN. The load generator
records the protocol it received, and the CSV confirms `HTTP/1.1` and
`HTTP/2.0` respectively. A later benchmark compares HTTP/1.1 and h2c directly.

| Configuration | wall (s) | files/s |
|---|---|---|
| edge, h1, one-shot PUT, 32 | 8.0–9.6 | 521–624 |
| edge, h2, one-shot PUT, 32 | 7.2–10.6 | 471–697 |
| edge, h1, session, 32 | 20.4–21.5 | 233–245 |
| edge, h2, session, 32 | 21.6–22.7 | 221–232 |

### Where the time goes inside a session

Sums across all workers, so they exceed wall time; the ratio is the point.

| Shape | hash | create | segment PUT | commit |
|---|---|---|---|---|
| logs, batch 100 | 0.1 s | 47 s | 193 s | 470 s |
| node-modules, batch 100 | 0.0 s | 31 s | 112 s | 1201 s |
| photos, 32 MiB segments | 1.9 s | 1.7 s | 27 s | 51 s |

## What dominates

**Per-file server work, not the network.** The logs corpus is 81 MiB. Uploading
it takes 6 seconds at best, which is 13 MiB/s on a loopback-class link that
moves 359 MiB/s for the same client and server when the files are large. Nothing
about the wire is the constraint; the cost is what the server does per file:
create the entry, write it, fsync it, fsync the directory, write the Pebble row
with `pebble.Sync`. 16 KiB of payload is a rounding error next to that.

**Concurrency is the one lever with a large effect,** and it saturates early:
1 → 8 in flight is 3.4x, 8 → 32 is another 1.3x, 32 → 64 is flat, and 128 is
worse than 64. The knee is around 32–64 in flight, which is roughly the server's
default segment-write slot budget (`NumCPU * 8`).

**Keep-alive is worth 1.4x when parallelism is low** (544–571 vs 359–403
files/s at 8 in flight) and disappears at 32, where enough requests are in
flight to hide connection setup. It is cheap insurance, not a headline.

**HTTP/2 changes nothing measurable.** Both protocols land inside each other's
spread, for whole-file PUTs and for sessions. That is the expected outcome: with
a keep-alive pool, HTTP/1.1 already has as many requests in flight as the server
will accept, and h2's wins (header compression, one connection) address costs
that are invisible next to an fsync. Note also that reaching h2 at all requires
a TLS terminator in front of Filegate.

**Batched session creation is not a throughput lever here.** It removes a third
of the requests (15000 → 10050 for 5000 files) but session creation is only
5–10% of session wall time, and the back-to-back comparison puts the two within
10% of each other. It is still the right API — it is one round trip instead of
N, which matters over a real WAN, and it rolls back atomically — but it is not
where folder-upload time goes.

**Upload sessions cost 2.5–3x a plain PUT for small files, and 1.6x for large
ones.** For the logs corpus: 780–815 files/s with `PUT /v1/paths` against
207–294 files/s with sessions. For the full-size photo folder: 359 MiB/s against
226 MiB/s. The reason is in the phase table — commit dominates every session
run, at 2.4–10x the segment upload time.

Commit is expensive because it stages, then copies. Segments are written into
`.fg-uploads/segments` (write, fsync, rename, directory fsync, Pebble row with
`Sync`), and then `assembleUploadSession` reads every segment back and writes the
whole file again into `.fg-uploads/complete` (fsync, rename, directory fsync)
before publishing it. Every byte is written twice and read once, plus a
per-segment checksum verification pass. For an 8.49 GiB photo folder that is
8.49 GiB of extra writes, which is exactly the 359 → 226 MiB/s gap.

**Signed direct upload URLs are free.** Two requests per file instead of one,
and the same 683–686 files/s as a plain PUT. Minting a URL is an HMAC, not an
fsync. Handing browsers direct URLs costs nothing on the server side.

## Packed segment assessment

The measurements do not support adding packed segments for small-file uploads.

Packing would coalesce many small files into few segment PUTs. Segment PUTs are
not the bottleneck: for the logs corpus they are 193 s of worker time against
470 s in commit, and for node-modules 112 s against 1201 s. Packing does not
touch commit, because each packed file still needs its own atomic publish, its
own directory fsync and its own index row — unless a session is allowed to carry
many objects and commit them together, which would be a different protocol.

The stronger argument is that for exactly the workload packing targets, the
session protocol should not be used at all. One-shot `PUT /v1/paths` is 2.5–3x
faster for small files and needs one request instead of three. It is not
resumable, but resuming a 16 KiB file is meaningless — retrying it costs one
round trip. Both SDKs already express this: the TypeScript SDK has
`directThresholdBytes` and the Go SDK now has `DirectThresholdBytes`. Building
packed segments would add protocol surface, a new failure mode (partial packs),
and new GC rules to a code path that small files should be skipping.

Two existing behaviors address this cost without expanding the protocol:

1. **Single-segment commit renames instead of copying.** A staged segment that
   has already been fsynced and verified can be published directly.
2. **The SDKs default `DirectThresholdBytes` to the segment size.** Folder
   uploads take the one-shot fast path for small files automatically.

## Concurrent directory creation observation

The benchmark recorded transient `404 {"error":"not found"}` responses when
many one-shot PUTs created files below new shared directories. The table records
the historical observation from this benchmark corpus.

Observed rate against a real containerized server, 5000 files per run:

| in flight | 404s per 5000 |
|---|---|
| 1, 8 | 0 |
| 32 | 0–1 |
| 64 | 1–33 |
| 128 | 22–95 |

It is not detector-specific: it reproduces with the detector parked at a 24h
poll interval. A 3s poll interval makes it much more likely (64 of 5000 in one
run), which fits an index-visibility race. The failures cluster at the start of a
run, when many workers are creating the same fresh directory tree.

It did not reproduce in-process: 2560 concurrent PUTs across 448 shared
directories through `httptest` produced zero errors in three attempts, so the
window appears to need real fsync latency to open. Reproduction is therefore
`bench/scripts/run-tree-bench.sh` with `--tree-shape logs --tree-transport put
--tree-files 64` or higher.

The SDK default of eight files in flight produced no failures in this run.
