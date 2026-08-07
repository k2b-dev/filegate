import { defaultPlugins, defineFibel } from "@k2b/fibel";
import {
  agentSkillsPlugin,
  assistantPlugin,
  mcpPlugin,
  providerFromEnv,
} from "@k2b/fibel/plugins";

const assistantPlugins = process.env.FIBEL_AI_MODEL?.trim()
  ? [
      assistantPlugin({
        provider: providerFromEnv(),
        launcherLabel: "Ask Filegate",
        systemPrompt: `
          Filegate is a Linux file gateway with REST, TypeScript, Go, and
          S3-compatible access. Answer only questions about integrating,
          configuring, operating, or developing Filegate. Use the
          documentation tools for procedural, configuration, API, code, or
          exact-behavior claims. Prefer concise, practical answers and clearly
          state when the documentation does not contain the requested
          information.
        `,
      }),
    ]
  : [];

export default defineFibel({
  title: "Filegate Docs",
  description: "Documentation for Filegate, a Linux file gateway with REST, SDK, and S3-compatible access.",
  siteUrl: "https://filegate.dev",
  locales: [{ code: "en", label: "English" }],
  defaultLocale: "en",
  routing: {
    basePath: "/docs",
    internalPath: "/_fibel",
    assetsPath: "/assets",
  },
  footerLinks: [
    { label: "GitHub", value: "https://github.com/k2b-dev/filegate" },
    { label: "LLM index", value: "https://filegate.dev/docs/en/llms.txt" },
  ],
  plugins: [
    ...defaultPlugins(),
    mcpPlugin(),
    agentSkillsPlugin({ directory: "agent-skills" }),
    ...assistantPlugins,
  ],
});
