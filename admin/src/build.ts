import { plugin } from "./config";

await Bun.build({
  entrypoints: ["src/server.tsx"],
  outdir: "dist",
  target: "bun",
  plugins: [plugin()],
});

await Bun.build({
  entrypoints: ["src/uploads.ts", "src/prompts.ts", "src/theme.ts", "src/settings.ts"],
  outdir: "dist",
  target: "browser",
});

await Bun.write("dist/styles.css", Bun.file("src/styles.css"));
