# Filegate documentation

Fibel serves the current root API, TypeScript/Go guides and operations reference.

```sh
bun install --frozen-lockfile
bun run typecheck
bun run build
bun run dev
```

`agent-skills/filegate` is the published copy of `../skills/filegate`; update both
together. The Fibel agent-skills plugin serves discovery at
`/.well-known/agent-skills/index.json`. No AI credentials are needed for a normal build;
the optional assistant activates only when `FIBEL_AI_MODEL` is configured.
