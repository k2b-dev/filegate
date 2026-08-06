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

## Agent integrations

The documentation server always exposes:

- a read-only MCP endpoint at `/docs/_fibel/mcp`;
- the self-contained Filegate skill through
  `/.well-known/agent-skills/`;
- setup instructions in the **Agents** footer dialog.

The documentation assistant is optional. It is enabled only when
`FIBEL_AI_MODEL` is non-empty, so local development and production deployments
without AI credentials continue to run with MCP and Agent Skills.

Copy `.env.example` to `.env` for local development or set the same variables
in the documentation server's runtime environment:

| Variable | Required | Meaning |
|---|---:|---|
| `FIBEL_AI_MODEL` | Yes, for the assistant | Provider model identifier. An empty value disables the assistant. |
| `FIBEL_AI_PROVIDER` | No | `openrouter` (default), `openai`, `anthropic`, `gemini`, `mistral`, or `ollama`. |
| `FIBEL_AI_BASE_URL` | No | Custom provider or OpenAI-compatible endpoint. |
| `OPENROUTER_API_KEY` | For OpenRouter | OpenRouter API key. |
| `OPENAI_API_KEY` | For OpenAI | OpenAI API key. |
| `ANTHROPIC_API_KEY` | For Anthropic | Anthropic API key. |
| `GEMINI_API_KEY` or `GOOGLE_API_KEY` | For Gemini | Gemini API key. |
| `MISTRAL_API_KEY` | For Mistral | Mistral API key. |

Ollama does not require an API key. Keep every provider credential in the
server environment; never expose it to the browser or commit it to the
repository.
