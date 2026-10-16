# Filegate

Filegate exposes ordinary Linux directories through a root-scoped HTTP API: one
Go daemon, its CLI, and TypeScript and Go clients. Applications own users and
authorization; Filegate owns roots, files, identities, versions and transfers.

The global agent defaults still apply. This file adds the context needed when
working on Filegate itself.

## Work as a Filegate maintainer

Always read the `filegate-dev` skill. It holds the architecture, the layer
owners and the invariants for identities, publication, ACLs, sessions and
leases. Use the `filegate` skill instead when the task only integrates or
operates an existing Filegate installation.

Before changing behavior:

1. Inspect `git status` and preserve unrelated or parallel work.
2. Read the public types in `api/v1`, the owning implementation and its focused
   tests, then the current documentation under `docs-site/docs/en`.
3. Name the owning layer and the observable behavior before choosing files.
4. Search every real caller of the affected contract, including both SDKs,
   before changing or deleting it.

## Work in Git

`main` is protected. Every change reaches it through a pull request that
passes the `gate` check and is squash-merged by the merge queue. Agents never
push to `main`, never create or move tags, and never trigger a release.

- Branch names are `type/short-slug` (`fix/recursive-search-pages`,
  `feat/search-match-mode`).
- Each piece of work gets its own worktree under `../filegate-wt/<slug>`,
  outside the repository:

  ```bash
  git fetch origin
  git worktree add ../filegate-wt/<slug> -b type/<slug> origin/main
  ```

- Commit as you go; commits need no approval. Pushing, opening the pull
  request, and enabling auto-merge are one block that needs one approval:
  `gh pr create --fill`, then `gh pr merge --auto`, which places the green
  pull request in the merge queue. The queue applies the squash, re-runs the
  gate on the merge result and merges. After the merge,
  `git worktree remove` the worktree; merged branches are deleted
  automatically.

The PR title becomes the squash commit and must be a Conventional Commit of
the form `type(scope): outcome`. `feat` is a minor release, `fix` a patch, and
`feat!` or a `BREAKING CHANGE:` footer a major release. Use `!` only with
explicit maintainer approval.

The maintainer cuts releases from `main` with a `v*` tag, which runs
`publish.yml`. Do not edit release versions in `README.md`,
`docs-site/docs/en/getting-started.md` or `sdk/ts/package.json` unless the
task is to prepare a release.

## Track work

GitHub Issues are the list of work: bugs, tracked debts, and concrete,
specified features that someone can pick up. A pull request closes its issue
with `Closes #n`. Dex holds the plan inside one piece of work: slices, status,
and handoff between sessions; a Dex task names its issue number and is
discarded after the merge.

- An issue describes one problem in at most four short sections: what is
  wrong, how to reproduce it (a command), the suspected cause with
  `file:line`, and a proposal. Analysis, plans, and progress belong in Dex.
- Use the "Agent report" issue form for issues an agent files; the "Bug
  report" form is for people.
- A one-commit fix needs neither an issue nor a Dex task, only a pull request.

## Keep the shared machine responsive

Development machines are often shared with other agents. Run the smallest
local check that can disprove the change, and leave the heavy suites to CI,
which runs all of them on every pull request:

- Locally: `go build` and `go vet` for the touched packages and focused
  `go test -count=1 -run <Test> ./<package>/`; prefer `nice` and `-p 2` when
  compiling a lot.
- Only when the maintainer asks, or when the change needs the seam and the
  machine is idle: `make test-linux` (Docker), `make test-race`, the full
  `make test`, `make docs`, `docker build`, and goreleaser package builds.
- Never leave daemons, containers or watchers running.

## Verify the seam you changed

Start with the fastest check that can disprove the change. Reproduce bugs
before fixing them when practical. A fix pull request answers "How do we
prevent this next time?" with a guard (check, test, or convention) or a reason
why none fits.

The `gate` check runs the Go build, vet and tests (including the root-only
Linux ACL suite in Docker), the TypeScript SDK build and tests, the
documentation build, the portable skill comparison, the container build,
package builds and a vulnerability scan. It runs on the pull request and
again in the merge queue, and its result is authoritative.

Every public contract change updates the TypeScript and Go clients, the Fibel
page and the skill references together; keep `skills/filegate` and
`docs-site/agent-skills/filegate` identical.

Review only the owned diff and run `git diff --check`. A finished handoff says
what behavior changed, which boundaries were checked, what ran, what passed,
and what could not be verified.
