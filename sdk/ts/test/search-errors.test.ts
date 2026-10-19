import { expect, test } from "bun:test";
import { Filegate, FilegateError } from "../src/index";

test("listing and search preserve busy and invalid observation errors", async () => {
  for (const [status, code] of [[429, "search_busy"], [409, "cursor_invalid"]] as const) {
    const request: typeof fetch = async () => Response.json({ error: code, message: "retry query" }, { status });
    const root = new Filegate({ baseUrl: "https://files.example", token: "backend", fetch: request }).root("documents");
    for (const call of [() => root.list("."), () => root.search("name")]) {
      try {
        await call();
        throw new Error("expected rejection");
      } catch (error) {
        expect(error).toBeInstanceOf(FilegateError);
        if (error instanceof FilegateError) {
          expect(error.status).toBe(status);
          expect(error.code).toBe(code);
          expect(error.message).toBe("retry query");
        }
      }
    }
  }
});
