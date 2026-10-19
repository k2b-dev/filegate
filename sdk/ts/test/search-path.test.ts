import { expect, test } from "bun:test";
import { Filegate, type ListingOptions } from "../src/index";

test("search forwards in and scope and preserves relative-path rune ranges", async () => {
  for (const selection of [undefined, "name", "path"] as const) {
    const response = { items: [{ path: "docs/Müller/2026/scan.pdf" }], matches: [[[0, 6], [7, 11]]] };
    const request: typeof fetch = async input => {
      const q = new URL(String(input)).searchParams;
      expect(q.get("in")).toBe(selection ?? null);
      expect(q.get("path")).toBe("docs");
      expect(q.get("q")).toBe("muller 2026");
      return Response.json(response);
    };
    const options: ListingOptions = { in: selection, sort: "relevance" };
    const root = new Filegate({ baseUrl: "https://files.example", token: "secret", fetch: request }).root("files");
    expect(await root.search("muller 2026", { ...options, path: "docs" })).toEqual(response);
  }
});
