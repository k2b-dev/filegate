import { createConfig } from "@valentinkolb/ssr";
import { createSSRHandler, routes } from "@valentinkolb/ssr/hono";

type PageOptions = {
  title?: string;
  theme?: "light" | "dark";
};

export const { config, plugin, html } = createConfig<PageOptions>({
  dev: process.env.NODE_ENV === "development",
  rootDir: import.meta.dir,
  template: ({ body, scripts, title, theme = "light" }) => `<!doctype html>
<html lang="en" class="${theme}" data-theme="${theme}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="view-transition" content="same-origin">
<meta name="theme-color" content="${theme === "dark" ? "#0f141b" : "#f2f3f5"}">
<title>Filegate Admin${title ? ` - ${title}` : ""}</title>
<link rel="preload" href="/fonts/tabler-icons.woff2" as="font" type="font/woff2" crossorigin>
<link rel="stylesheet" href="/tabler-icons.css">
<link rel="stylesheet" href="/styles.css">
<script type="module" src="/theme.js"></script>
</head>
<body>${body}${scripts}</body>
</html>`,
});

export const ssr = createSSRHandler(html);
export { routes };
