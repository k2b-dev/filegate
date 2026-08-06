# Filegate documentation site

This package is the canonical English documentation source for `filegate.dev`.
Product, operator, API, development, and benchmark documentation all live under
`docs/en`; do not add a second long-form documentation tree at the repository
root.

```sh
bun install --frozen-lockfile
bun run typecheck
bun run dev
bun run build
```

The public docs are mounted under `/docs`.
