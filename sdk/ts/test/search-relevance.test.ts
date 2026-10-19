import { expect, test } from "bun:test";
import { Filegate, type MatchRange, type ListingOptions } from "../src/index";

test("relevance forwards options and decodes search metadata", async () => {
  const response = { items: [{ path: "Müller.pdf" }], total: 42, truncated: true, matches: [[[0, 6]]], next: "cursor" };
  const request: typeof fetch = async (input) => {
    const q = new URL(String(input)).searchParams;
    expect(q.get("sort")).toBe("relevance");
    expect(q.get("order")).toBe("desc");
    expect(q.get("q")).toBe("muller");
    expect(q.get("after")).toBe("previous");
    return Response.json(response);
  };
  const root = new Filegate({ baseUrl: "https://files.example", token: "secret", fetch: request }).root("docs");
  const options: ListingOptions = { sort: "relevance", order: "desc", after: "previous" };
  const page = await root.search("muller", options);
  const range: MatchRange | undefined = page.matches?.[0]?.[0];
  expect(range).toEqual([0, 6]);
  expect(page).toEqual(response);
});

test("empty search metadata distinguishes complete and keyset pages", async () => {
  for (const response of [{ items: [], total: 0, truncated: false, matches: [] }, { items: [], matches: [] }]) {
    const request: typeof fetch = async () => Response.json(response);
    const root = new Filegate({ baseUrl: "https://files.example", token: "secret", fetch: request }).root("docs");
    expect(await root.search("missing")).toEqual(response);
  }
});
