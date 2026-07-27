import { plugin } from "./config";

await Bun.build({
  entrypoints: ["src/server.tsx"],
  outdir: "dist",
  target: "bun",
  plugins: [plugin()],
});

await Bun.build({
  entrypoints: ["src/uploads.ts", "src/prompts.ts", "src/theme.ts", "src/s3.ts", "src/system.ts", "src/toast.ts"],
  outdir: "dist",
  target: "browser",
});

await Bun.write("dist/styles.css", Bun.file("src/styles.css"));

// The icon font is self-hosted. An admin panel for a self-hosted file gateway
// must not depend on a CDN at runtime: it may well run on a network with no
// route to the internet.
const iconFontDir = "node_modules/@tabler/icons-webfont/dist";
// Rewrite the @font-face: the shipped rule also lists woff and ttf, which we do
// not serve, and font-display: block keeps a missing glyph blank instead of
// flashing tofu boxes while the font loads.
const iconCSS = await Bun.file(`${iconFontDir}/tabler-icons.min.css`).text();
await Bun.write(
  "dist/tabler-icons.css",
  iconCSS.replace(
    /@font-face\{[^}]*\}/,
    '@font-face{font-family:"tabler-icons";font-style:normal;font-weight:400;font-display:block;src:url("/fonts/tabler-icons.woff2") format("woff2")}',
  ),
);
await Bun.write("dist/fonts/tabler-icons.woff2", Bun.file(`${iconFontDir}/fonts/tabler-icons.woff2`));
