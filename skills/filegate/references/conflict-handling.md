# Conflict Handling

Filegate never silently overwrites or drops data. Every write that can collide on a name accepts an `onConflict` argument; the default is **always** `error`. This is uniform across endpoints — once you understand it for one, you know it for all.

## The four modes

| Mode        | Behavior                                                                                            |
|-------------|-----------------------------------------------------------------------------------------------------|
| `error`     | **Default.** Returns `409 Conflict` if the target already exists.                                   |
| `overwrite` | Replace the existing file. For `PUT /v1/paths` and upload-session commit on a file target, the existing node ID is preserved. For `transfer move`, the **source** ID is preserved across the move; the **target's** old ID is gone. For `transfer copy`, the copied node always gets a fresh ID. |
| `rename`    | Pick a unique sibling name (`foo.jpg` → `foo-01.jpg`, `foo-02.jpg`, …). New node, new name.        |
| `skip`      | Mkdir-only. If a directory with the same name exists, return it unchanged.                          |

## Endpoint matrix

| Endpoint                         | Default | Allowed                | Notes                                                          |
|----------------------------------|---------|------------------------|----------------------------------------------------------------|
| `PUT /v1/paths/{path}`           | error   | error/overwrite/rename | Query string `?onConflict=...`. `error`/`overwrite` cannot replace a directory with a file (returns 409). `rename` succeeds by creating a new file at a unique sibling name (the directory is left untouched). |
| `POST /v1/nodes/{id}/mkdir`      | error   | error/skip/rename      | Body field. `overwrite` is rejected (use Transfer for that). Intermediate path segments are always reuse-if-dir. |
| `POST /v1/uploads/sessions`      | error   | error/overwrite        | Body field. Create does an optimistic check; commit repeats the authoritative check. |
| `POST /v1/transfers`             | error   | error/overwrite/rename | Body field. (No `ensureUniqueName` field exists — for that semantic use `onConflict: "rename"`.) |

## Rename semantics — works for every type combination

`rename` always succeeds by picking a fresh sibling, regardless of what's at the target:

| You want to write... | Target has...   | Result                                         |
|----------------------|-----------------|------------------------------------------------|
| File                 | (nothing)       | File at original name                          |
| File                 | File            | File at `<stem>-NN<ext>`                       |
| File                 | Directory       | File at `<stem>-NN<ext>` (dir untouched)       |
| Directory (mkdir)    | (nothing)       | Directory at original name                     |
| Directory (mkdir)    | Directory       | Directory at `<stem>-NN`                       |
| Directory (mkdir)    | File            | Directory at `<stem>-NN` (file untouched)      |

Suffix counter goes `01..99..999`, then falls back to a millisecond-timestamp suffix to guarantee termination. Not configurable — keep it simple.

## 409 response body — diagnostic fields are best-effort

When the conflict comes from a **name-collision check** (`PUT /v1/paths`,
`POST /v1/nodes/{id}/mkdir`, or upload-session create with the
optimistic check), the daemon enriches the 409 body with diagnostic
fields:

```json
{
  "error": "filename already exists in parent",
  "existingId": "01933abc-...",
  "existingPath": "data/users/alice/photo.jpg"
}
```

Transfer 409s now also carry the diagnostic fields when the daemon can
resolve the colliding child (the common case). The remaining endpoints
that may return only the generic envelope are **segment
duplicate-content rejections** and a few generic
fallback paths:

```json
{ "error": "conflict" }
```

So `existingId` / `existingPath` may be empty even on 409. Code
defensively: render the diagnostic prompt only when the fields are
present, and fall back to a generic "this conflicts with an existing
node" message otherwise.

## TS SDK usage

```ts
import { FilegateError } from "@k2b/filegate/client";

try {
  await fg.paths.put("photos/sunset.jpg", bytes);
} catch (e) {
  if (e instanceof FilegateError && e.status === 409 && e.errorResponse) {
    const choice = await askUser({
      message: `Datei ${e.errorResponse.existingPath} existiert bereits.`,
      options: ["overwrite", "rename", "cancel"],
    });
    if (choice === "cancel") return;
    await fg.paths.put("photos/sunset.jpg", bytes, { onConflict: choice });
  } else throw e;
}
```

`e.errorResponse` is the parsed `{ error, existingId?, existingPath? }`
envelope (present when the body was JSON in the documented shape). The
raw body string is on `e.body` if you ever need it for debugging.

## Go SDK usage

The catch on retry: `Paths.Put` consumes the `io.Reader`. To retry with a
different `onConflict` you have to give the SDK a fresh reader at offset
0 — either reopen the file, or use `io.SeekStart` on a `*os.File` /
`*bytes.Reader`:

```go
src, err := os.Open("/local/photo.jpg")
if err != nil { return err }
defer src.Close()

put := func(mode filegate.FileConflictMode) (*filegate.PathPutResponse, error) {
    if _, err := src.Seek(0, io.SeekStart); err != nil { return nil, err }
    return fg.Paths.Put(ctx, "photos/sunset.jpg", src, filegate.PutPathOptions{
        ContentType: "image/jpeg",
        OnConflict:  mode,
    })
}

_, err = put(filegate.ConflictError)
if err != nil {
    var fe *filegate.APIError
    if errors.As(err, &fe) && fe.IsConflict() {
        choice := promptUser(fe.ExistingPath)        // may be "" if generic 409
        if choice == "cancel" { return nil }
        _, err = put(filegate.FileConflictMode(choice))   // "overwrite" or "rename"
    }
}
```

If your reader is a `bytes.Buffer` or a network stream that can't seek,
you have to materialize the bytes once (or re-fetch them) before the
retry — silently retrying with a consumed reader sends an empty body.

## The upload-session subtlety — create vs commit

For upload sessions, the conflict check runs in **two places**:

1. **Optimistic check at create** — if the name already exists and mode is `error`, you get 409 immediately. Zero segments uploaded. This is the common case and saves bandwidth on retries.
2. **Authoritative check at commit** — even after a clean create, another writer might create the target before you commit. The persisted `onConflict` mode is consulted again.

If the persisted mode is `error` and commit hits a collision, the response is
409. Create a new session with the desired `onConflict` rule and re-upload
the missing segments, or let the user choose a new path.

Upload sessions do not support `rename`; resumable retry recovery needs one
stable target path. Use `error` and let the application choose a new path, or
use `overwrite` explicitly.

```ts
const session = await fg.uploads.sessions.create({ ...base, onConflict: "overwrite" });
for (const segment of session.segments) {
  await fg.uploads.sessions.segments.put({ sessionId: session.id, ...segmentBody(segment) });
}
await fg.uploads.sessions.commit({ sessionId: session.id });
```

This is what makes the optimistic-then-authoritative scheme usable. You're never stuck.

## Anti-patterns

- **Don't** catch all 409s and silently retry with `overwrite`. That defeats the purpose of the safe default. Always involve the user (or a clearly-documented business rule).
- **Don't** pre-flight check existence with `GET /v1/paths/...` and then PUT without `onConflict`. The TOCTOU window between your GET and PUT is exactly what `onConflict: "error"` is designed to handle atomically.
- **Don't** use `overwrite` for "I want to make sure this folder exists" — use `mkdir` with `onConflict: "skip"` instead. `overwrite` is rejected for mkdir on purpose.
