---
title: Browse and measure directories
section: Use
order: 15
description: Sort and filter before paging, handle invalid cursors and interpret directory totals.
---

# Browse and measure directories

Use `list` for immediate children of one directory, such as a folder tree or a
destination picker. Use `search` to match filenames below a path. Search includes
descendants and excludes the base directory itself.

```ts
const options = { sort: "name", order: "asc", type: "directories", limit: 100 } as const;
let page = await root.list("teams", options);
while (page.next !== undefined) {
  page = await root.list("teams", { ...options, after: page.next });
}
```

Sorting and filtering apply before pagination. Equal sort values use the relative
path as a stable tie-breaker. Descending order reverses both the primary order
and the tie-breaker.

A listing's `revision` can be absent or reflect an earlier indexed observation.
Before binding an `ifMatch` upload, read the current `stat` result or the ETag from
the current-content response. Do not use listing metadata as a fresh revision.

| Option | Values and default |
| --- | --- |
| `sort` | `path` (default), `name`, `size`, `modified`; search also accepts `relevance` with non-empty `q`. |
| `order` | `asc` (default), `desc`. |
| `type` | `all` (default), `files`, `directories`. |
| `match` | `exact` (default), `fuzzy`. |
| `limit` | 1–1000; default 100. |
| `maxEntries` | 1–100000; default 100000. |
| `after` | The previous response's opaque `next` cursor. |

Keep every option unchanged when following a cursor. Continue until `next` is
absent, including when an indexed page contains no items: a bounded scan may
find no matches yet still have more entries to examine.

## Find filenames

Search normalizes Unicode forms, ignores case with full case folding (`ß` = `ss`),
and folds accents (`muller` finds `Müller.docx`). These rules apply with the default
`match: "exact"` too. Underscores, hyphens, dots and whitespace are word gaps;
`quartalsbericht` finds `Quartals_Bericht.xlsx` with the gaps removed.

The raw `q` value is limited to 8 KiB (8192 UTF-8 bytes) before normalization
and has at most eight whitespace-separated tokens. Every token must match,
in any order: `2026 bericht` finds `Quartalsbericht 2026.pdf`. Exceeding either
query limit or using an unknown `match` value returns 400 (`invalid_argument`).

Use `match: "fuzzy"` to include typos and loose abbreviations:

```ts
const page = await root.search("tsettest", { match: "fuzzy", path: "reports" });
```

The HTTP equivalent is `GET /v1/roots/{root}/search?q=tsettest&match=fuzzy`.
In Go, pass `filegate.ListingOptions{Match: "fuzzy"}` to `Search`.

Each token uses the strongest matching tier:

1. **Exact:** normalized equality, prefix, word start or substring, including a
   match with word gaps removed.
2. **Typo:** an approximate substring with optimal string alignment distance:
   one insertion, deletion, substitution or adjacent transposition for tokens of
   4–7 runes; up to two edits for tokens of 8–32 runes. Tokens outside the 4–32
   rune range use exact and loose matching only. `tsettest` and `tete` both find
   `testtest.ods`.
3. **Loose:** the token's runes occur in order. Tokens need at least two runes,
   and the span from the first to the last matched rune must be at most three
   times the token length. `tst` finds `testtest.ods` this way.

A filename's tier is its weakest token's tier. With `sort: "relevance"`, loose
hits always appear after exact and typo hits. With other sorts, loose hits appear
only when the complete selection has no exact or typo hit. This rule applies
after the path and type filters, before pagination. `match: "exact"` includes
only the first tier.

## Show the best matches first

Use `sort: "relevance"` with a non-empty `q`. Filegate orders results by:

1. Tier: exact, then typo, then loose.
2. Sub-rank, summed across tokens in the filename's weakest tier. Exact uses
   equality (0), prefix (1), word start (2), substring (3); typo sums edit
   distances. Loose first prefers fewer tokens that start inside a word, then
   the smaller sum of matched spans.
3. No accent folding needed, then accent folding needed.
4. Shorter basename in runes, fewer path segments, then path in byte order.

`order` is ignored for relevance; results always run from best to worst.
An empty or whitespace-only `q` with relevance returns 400 (`invalid_argument`).
Directory listings do not accept relevance.

Relevance keeps only the best K hits, default 1000. Operators can change K with
[`search.topK`](/docs/en/configuration). `total` counts the complete selection
before this cap; `truncated: true` means some hits are outside the retained K.
Only retained hits are pageable. Other sorts have no top-K cap.

Every search page with a non-empty `q` includes `matches`, aligned with `items`.
Each entry is an array of `[start, end)` rune ranges into the original basename.
Exact hits highlight matching runes, typo hits the matched approximate substring,
and loose hits the individual matched runes. Adjacent or overlapping ranges merge.
Offsets refer to the original spelling, including composed or decomposed accents
and full case folds; they are not UTF-8 byte or JavaScript UTF-16 offsets.

```ts
const page = await root.search("tst", { match: "fuzzy", sort: "relevance" });
// For items[i].path === "reports/testtest.ods":
// matches[i] === [[0, 1], [2, 4]] highlights t + st.
const basename = page.items[0]?.path.split("/").at(-1) ?? "";
const runes = Array.from(basename);
const fragments = page.matches?.[0]?.map(([start, end]) => runes.slice(start, end).join(""));
```

`total` and `truncated` are present when Filegate collected the complete
selection into an observation, including empty-query searches on that path.
Keyset-paged exact searches omit both fields, even on the last page. A missing
`total` means unknown. Directory listing responses retain `{items, next?}`.

## Handle changing directories

Indexed queries reflect the configured index, including its freshness limits for
external writers. Run a rebuild to include external changes. Searches below a
non-root path with `sort: "name"`, `"size"` or `"modified"` collect and sort the
selected subtree once. Fuzzy and relevance searches collect and sort once for
every path. These scan budgets apply only to the selected subtree. Other indexed
queries seek to the next position instead of rescanning earlier pages.

Filesystem queries, sorted indexed subtree searches, fuzzy and relevance searches
use a bounded temporary observation. The observation lasts 60 seconds. Each root
retains at most 16 observations and 32 MiB of listing data. Live filesystem scans
and indexed scans with other sorts accept at most `maxEntries` visited entries
(default and maximum: 100000). Indexed relevance scans visit the complete subtree
and retain at most K hits; `maxEntries` caps retained hits on this path instead
of visited entries. Setting it below K can return 413 if too many hits need to
be retained. Cancellation still stops the scan. Exceeding a visit or memory
budget returns 413 instead of a partially sorted result.

Filegate mutations, an index rebuild, restart, changed query options, observation
expiry or capacity eviction can invalidate a cursor. On `409 cursor_invalid`,
discard the accumulated pages and restart the query. Indexed first pages return
their captured observation even if a Filegate mutation or rebuild occurs during
the scan. Any returned cursor stays bound to that observation, so following it
after such a change returns `409 cursor_invalid`. Continuation pages also return
this error if the root changes while they scan. Indexed scans allow writes to
proceed. Live filesystem scans retain the root lock to coordinate identity and
revision updates with permission checks. Filesystem observations are not atomic
snapshots of external writers.

Each root admits at most two searches at once, shared across Unix execution
identities. Directory listings do not consume search slots. A further search
waits up to 200 ms, then returns
`429 search_busy` with `Retry-After: 1` if both slots remain occupied. Retry the
same request after that delay. Active scans check cancellation between entries
and release their slots when they exit. A search waiting for the root lock
retains its slot until that lock becomes available.

Unix execution identities are supported for both listing and search. Scoped
queries always use the live filesystem, including on indexed roots; they never
fall back to unfiltered index results. A new query without a cursor observes
external NFS changes without requiring a rebuild. Continuations retain the
initial observation and remain bound to the original identity and options.

A recursive scan that encounters an unreadable subtree fails the whole query,
even when that subtree would not match the search text. It does not return a
successful partial result. Each page rechecks base-directory access, traversal
to each returned entry and read permission on its parent. An entry deleted by an
external writer makes the page fail with 404; revoked access returns 403. Neither
returns a partial page. Restart the query to obtain a new observation. A readable
directory can expose a file's name and metadata without granting access to its
bytes; download checks content read permission separately.

## Read recursive totals

```ts
const totals = await root.recursiveStats("teams/editors", 100000);
if (totals.complete && totals.freshness === "observed") {
  console.log(totals.files, totals.bytes, totals.started, totals.completed);
}
```

This performs one bounded filesystem walk of the selected subtree. A selected
directory counts toward `directories`. The result contains `path`, `files`,
`directories`, logical `bytes`, `started`, `completed`, `updated`, `complete`,
`source` and `freshness`, with optional `indexBuilt` for index observations.
A budget-limited subtree result has `complete: false`; its counts are partial.

`freshness: "observed"` describes the scan interval, not a guarantee that nothing
changed afterward or during an external write. Cached index totals use
`freshness: "unknown"`. Missing totals are `null`, not zero. None of these values
is an atomic storage quota. The application must reserve upload budgets itself.

`root.stats()` reads the cached root totals. `refreshStats()` updates that cache
with a full bounded root walk and fails with 413 if incomplete. `recursiveStats`
does not replace the root cache and can use a Unix execution identity. Cached
root totals and root-wide refresh are administrative and reject that identity.
