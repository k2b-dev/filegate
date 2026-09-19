---
title: Browse and measure directories
section: Use
order: 15
description: Sort and filter before paging, handle invalid cursors and interpret directory totals.
---

# Browse and measure directories

Use `list` for immediate children of one directory, such as a folder tree or a
destination picker. Use `search` for case-insensitive filename matching below a
path. Search includes descendants and excludes the base directory itself.

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
| `limit` | 1–1000; default 100. |
| `maxEntries` | 1–100000; default 100000. |
| `after` | The previous response's opaque `next` cursor. |

Keep every option unchanged when following a cursor. Continue until `next` is
absent, including when an indexed page contains no items: a bounded scan may
find no matches yet still have more entries to examine.

## Handle changing directories

Indexed queries seek to the next position instead of rescanning earlier pages.
They reflect the configured index, including its freshness limits for external
writers. Run a rebuild to include external changes.

Filesystem queries scan and sort once into a bounded temporary observation.
The observation lasts 60 seconds. Each root retains at most 16 observations and
32 MiB of listing data; each scan accepts at most 100000 entries. If the scan
budget is exceeded, the request returns 413 instead of a partially sorted result.

Filegate mutations, an index rebuild, restart, changed query options, observation
expiry or capacity eviction can invalidate a cursor. On `409 cursor_invalid`,
discard the accumulated pages and restart the query. Filesystem observations
are not atomic snapshots of external writers.

A Unix execution identity is supported for directory listing. It uses a live
filesystem observation even on indexed roots, and access is checked again for
each page. Search does not accept that identity. Do not treat
administrative search results as Unix permission-filtered results.

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
does not replace the root cache. Stats operations are administrative and reject
Unix execution identities.
