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
| `in` | Search basename with `name` (default), or scope-relative `path`. |
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
query limit or using an unknown `match` or `in` value returns 400 (`invalid_argument`).

Use `in: "path"` to include folder names:

```ts
const page = await root.search("rechnung 2026", { in: "path", path: "docs" });
// Finds docs/Rechnungen/2026/scan01.pdf.
```

The HTTP equivalent is
`GET /v1/roots/{root}/search?q=rechnung%202026&in=path&path=docs`.
In Go, pass `filegate.ListingOptions{In: "path"}` to `Search`.
Every token can match any segment of the path relative to the search base.
The base itself is excluded: searching below `docs` does not match `docs`
against its own prefix. `/` is a word gap, including for matching with gaps
removed. The default `in: "name"` searches only the basename.

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

With `in: "path"`, typo and loose matching run only on normalized path forms
of at most 256 runes. Longer forms use exact matching only, including hits near
the end of the path. The cap applies separately to each form (with or without
accents and word gaps); the 32-rune typo token cap and 8 KiB query cap still apply.
This means `in: "path"` can find fewer fuzzy hits than `in: "name"` for deep
paths. Use `in: "name"` when only basename typo or abbreviation matching matters.

A result's tier is its weakest token's tier. With `sort: "relevance"`, loose
hits always appear after exact and typo hits. With other sorts, loose hits appear
only when the complete selection has no exact or typo hit. This rule applies
after all filters, before pagination. `match: "exact"` includes
only the first tier.

## Show the best matches first

Use `sort: "relevance"` with a non-empty `q`. Filegate orders results by:

1. Tier: exact, then typo, then loose.
2. Sub-rank, summed across tokens in the result's weakest tier. Exact uses
   equality (0), prefix (1), word start (2), substring (3); typo sums edit
   distances. Loose first prefers fewer tokens that start inside a word, then
   the smaller sum of matched spans.
3. With `in: "path"`, fewer tokens without a basename hit at their selected
   tier or better. Count all tokens, including those in stronger tiers. A weaker
   basename hit does not promote a stronger parent hit.
4. No accent folding needed, then accent folding needed.
5. Shorter basename in runes, fewer path segments, then path in byte order.

Exact equality and prefix refer to the whole selected string: the basename by
default, or the relative path with `in: "path"`. A first-folder prefix can
therefore outrank a basename word-start hit, because sub-rank is compared first.

`order` is ignored for relevance; results always run from best to worst.
An empty or whitespace-only `q` with relevance returns 400 (`invalid_argument`).
Directory listings do not accept relevance.

Relevance keeps only the best K hits, default 1000. Operators can change K with
[`search.topK`](/docs/en/configuration). `total` counts the complete selection
after all filters and before this cap; `truncated: true` means some hits are
outside the retained K.
Only retained hits are pageable. Other sorts have no top-K cap.

Every search page with a non-empty `q` includes `matches`, aligned with `items`.
Each entry is an array of `[start, end)` rune ranges into the original basename
with `in: "name"`. With `in: "path"`, offsets refer to `items[i].path` relative
to the normalized search base, without a leading slash. For base `docs` and node
`docs/Müller/2026/scan.pdf`, the offset string is `Müller/2026/scan.pdf`;
`muller 2026` highlights `[[0, 6], [7, 11]]`. At the root base (`.`), use the
returned path as-is. `Node.path` always remains root-relative.
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

## Filter search results

Search accepts these additional filters, with or without `q`. Combine them with
`type` and any supported sort. They apply before name matching, relevance scoring
and top-K selection; directory listings do not use them.

| Option | Values |
| --- | --- |
| `ext` | Comma-separated extensions without dots, such as `pdf,docx`; case-folded (`ß` = `ss`). |
| `minSize`, `maxSize` | Inclusive byte bounds: non-negative 64-bit integers, with `minSize <= maxSize`. |
| `modifiedAfter` | RFC 3339 timestamp, inclusive. |
| `modifiedBefore` | RFC 3339 timestamp, exclusive. |

Whitespace around extension values is trimmed: `ext=pdf, docx` works like
`ext=pdf,docx`. HTTP also accepts repeated parameters: `ext=pdf&ext=docx`.

`ext` matches only the basename's final extension: `a.tar.gz` matches `gz`,
while `.bashrc`, `README` and `name.` have no extension. Any extension or size
filter excludes directories. Time filters apply to both files and directories.
Empty extension values, values containing dots, invalid sizes or invalid
timestamps return 400 (`invalid_argument`). Timestamps must stay within the four-digit year range
(0000–9999) after UTC conversion. Encode `+` in HTTP timestamp offsets as `%2B`.

Find PDFs of at least 10 MiB modified during 2026 without a filename query:

```ts
const page = await root.search("", {
  ext: "pdf",
  minSize: 10485760,
  modifiedAfter: "2026-01-01T00:00:00Z",
  modifiedBefore: "2027-01-01T00:00:00Z",
});
```

The HTTP equivalent is
`GET /v1/roots/{root}/search?ext=pdf&minSize=10485760&modifiedAfter=2026-01-01T00:00:00Z&modifiedBefore=2027-01-01T00:00:00Z`.
TypeScript uses timestamp strings. In Go, set `ListingOptions.Ext` to
`[]string{"pdf"}`, size bounds to `*int64` and time bounds to `*time.Time`;
`nil` omits a bound, while a size pointer to zero includes that bound.

Keep the filters when following a cursor. Extension order, duplicates and case,
and equivalent timestamp offsets normalize to the same selection. Changing a
filter invalidates the cursor.

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
be retained. Indexed relevance scans also have a server-owned 10-second time
budget. Cancellation still stops the scan. Exceeding a visit, memory or scan-time
budget returns 413 (`limit_exceeded`) instead of a partially sorted result;
failed first pages leave no cached observation. Narrow the search path or query
before retrying.

Filegate mutations, an index rebuild, restart, changed query options, observation
expiry or capacity eviction can invalidate a cursor. On `409 cursor_invalid`,
discard the accumulated pages and restart the query. Indexed first pages return
their captured observation even if a Filegate mutation or rebuild occurs during
the scan. Any returned cursor stays bound to that observation, so following it
after such a change returns `409 cursor_invalid`. Continuation pages also return
this error if the root changes while they scan. Indexed scans allow writes to
proceed. Relevance pages also return `409 cursor_invalid` if a retained hit is
missing, has changed between a file and a directory, or no longer satisfies the
search filters. This includes external deletion or rename. Live filesystem scans
retain the root lock to coordinate identity and revision updates with permission
checks.
Filesystem observations are not atomic snapshots of external writers.

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
external writer makes a relevance page fail with `409 cursor_invalid`; other
sorts return 404. Revoked access returns 403. These errors return no partial page.
Restart the query to obtain a new observation. A readable directory can expose a
file's name and metadata without granting access to its bytes; download checks
content read permission separately.

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
