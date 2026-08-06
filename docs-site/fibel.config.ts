import { defineFibel } from "@k2b/fibel";

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
    { label: "GitHub", value: "https://github.com/valentinkolb/filegate" },
    { label: "Raw index", value: "/en.md" },
  ],
});
