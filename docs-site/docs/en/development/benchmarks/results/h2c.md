---
title: HTTP/1.1 compared with h2c
navTitle: HTTP/1.1 and h2c
section: Benchmark results
order: 440
description: Controlled Filegate listener measurements for HTTP/1.1 and cleartext HTTP/2.
tags: [benchmarks, http2, performance]
---

# HTTP/1.1 against h2c on the same listener

Raw data: `tree-bench-20260727_012223.csv`, `tree-bench-20260727_012612.csv`.
Harness: `bench/scripts/run-tree-bench.sh` with `FILEGATE_BENCH_PRESET=h2c`.

Ticket `micao586` asked for one of two things: support h2c on the plain listener,
or document that the listener is HTTP/1.1 only. h2c was added, so this is the
rerun the ticket required — confirm it changes nothing for small files and does
not regress large ones.

It confirms both.

## Why this measurement replaces the earlier one

`2026-07-26-many-small-files.md` compared HTTP/1.1 against HTTP/2 through a Caddy
edge that terminated TLS, because the listener had no h2c and there was no other
way to get HTTP/2 to it. That comparison carried the proxy in both arms.

These pairs hit the listener directly on the same port, with no proxy in between,
so the only difference is the protocol.

## Setup

Same host and harness as the earlier results: Apple M1 Max, Docker Desktop Linux
VM, ext4 on a named volume, detector parked, versioning off, stack recreated
between every run. Two runs of the preset, two repeats each, so four samples per
configuration. `FILEGATE_SERVER_HTTP2_CLEARTEXT` is on for the whole preset; the
arms differ only in whether the client asks for HTTP/2.

The client is configured with `UnencryptedHTTP2` and without `HTTP1`, so it cannot
silently fall back. Every h2c row in the CSV records `proto=HTTP/2.0`, which is
the check that the arms are really different.

## Results

| Configuration | Protocol | median | range | errors |
|---|---|---:|---|---:|
| logs, one-shot PUT, 32 | HTTP/1.1 | 636 files/s | 601–723 | 0 |
| logs, one-shot PUT, 32 | h2c | 626 files/s | 563–725 | 0 |
| logs, session, 32, batch 100 | HTTP/1.1 | 351 files/s | 288–374 | 0 |
| logs, session, 32, batch 100 | h2c | 327 files/s | 280–377 | 0 |
| photos, one-shot PUT, 8 | HTTP/1.1 | 414 MiB/s | 240–628 | 0 |
| photos, one-shot PUT, 8 | h2c | 404 MiB/s | 240–429 | 0 |

Median differences are −1.5%, −6.7% and −2.6%. Every one of them is inside the
spread of its own arm: the HTTP/1.1 photos arm alone ranges 240–628 MiB/s, a
factor of 2.6 for identical work. There is no measurable difference, which matches
what the edge comparison found.

## A first run that said otherwise

The first run of the session pair looked like a clean regression: h2c at 280–300
files/s against HTTP/1.1 at 360–374, all four samples separated, which is the
shape of a real signal rather than noise. There was even a plausible mechanism —
HTTP/2 multiplexes onto one connection where HTTP/1.1 opens a socket per
concurrent request, and against a local server there is no handshake cost to
amortize.

The second run reversed it: h2c 354–377 against HTTP/1.1 288–341. The mechanism
was a story fitted to four numbers.

Recorded because it is the failure mode this host invites. Two repeats of one
configuration are not enough to separate a 20% effect here, however cleanly they
appear to line up.

## Large files do not regress

The photos arm is the large-file check: 150 files, 2.3 GiB, median 414 MiB/s on
HTTP/1.1 and 404 on h2c. The h2c arm's best sample is lower (429 against 628), but
that 628 is a single high outlier and the medians are 2.6% apart.

## What this does not measure

Concurrency past 32 in flight. HTTP/2's single connection could plausibly become
a bottleneck where HTTP/1.1's pool would not, and the earlier work found the knee
for this server at 32–64 in flight. Nothing here rules that out; it was not the
question the ticket asked, and h2c is off by default.
