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
| `sort` | `path` (default), `name`, `size`, `modified`. |
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

The query has at most eight whitespace-separated tokens. Every token must match,
in any order: `2026 bericht` finds `Quartalsbericht 2026.pdf`. More than eight
tokens or an unknown `match` value returns 400.

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
   4–7 runes; up to two edits for tokens of 8 or more runes. Shorter tokens never
   get typo matching. `tsettest` and `tete` both find `testtest.ods`.
3. **Loose:** the token's runes occur in order. Tokens need at least two runes,
   and the span from the first to the last matched rune must be at most three
   times the token length. `tst` finds `testtest.ods` this way.

A filename's tier is its weakest token's tier. With every supported sort, loose
hits appear only when the complete selection has no exact or typo hit. This
rule applies after the path and type filters, before pagination. Fuzzy searches
collect the complete selection once; they keep your requested sort and order.
`match: "exact"` includes only the first tier.

## Handle changing directories

Indexed queries reflect the configured index, including its freshness limits for
external writers. Run a rebuild to include external changes. Searches below a
non-root path with `sort: "name"`, `"size"` or `"modified"` collect and sort the
selected subtree once. Fuzzy searches collect and sort once for every path and
sort. These scan budgets apply only to the selected subtree. Other indexed
queries seek to the next position instead of rescanning earlier pages.

Filesystem queries, sorted indexed subtree searches and all fuzzy searches use
a bounded temporary observation. The observation lasts 60 seconds. Each root
retains at most 16 observations and 32 MiB of listing data; each scan accepts at most
`maxEntries` entries (default and maximum: 100000). If the scan budget is exceeded,
the request returns 413 instead of a partially sorted result.

Filegate mutations, an index rebuild, restart, changed query options, observation
expiry or capacity eviction can invalidate a cursor. On `409 cursor_invalid`,
discard the accumulated pages and restart the query. Filesystem observations
are not atomic snapshots of external writers.

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
