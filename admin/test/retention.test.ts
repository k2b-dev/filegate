import { describe, expect, test } from "bun:test";
import { bucketsToText, textToBuckets, tidyDuration, type Bucket } from "../src/retention";

describe("duration tidying", () => {
  test("drops the zero components Go emits", () => {
    expect(tidyDuration("720h0m0s")).toBe("720h");
    expect(tidyDuration("1h0m0s")).toBe("1h");
    expect(tidyDuration("15m0s")).toBe("15m");
    expect(tidyDuration("1h30m0s")).toBe("1h30m");
    expect(tidyDuration("3s")).toBe("3s");
  });

  test("leaves anything it cannot parse alone rather than guessing", () => {
    expect(tidyDuration("nonsense")).toBe("nonsense");
    expect(tidyDuration("")).toBe("");
  });
});

describe("policy text round trip", () => {
  const policy: Bucket[] = [
    { keepFor: "1h", maxCount: -1 },
    { keepFor: "24h", maxCount: 24 },
    { keepFor: "720h", maxCount: 30 },
  ];

  test("serializes with separators", () => {
    const text = bucketsToText(policy);
    // The value also lands in a single-line input, where a newline would
    // collapse and run the rules together.
    expect(text).toContain("; ");
    expect(text.split("; ")).toHaveLength(3);
    expect(text).toBe("keep_for=1h,max_count=-1; keep_for=24h,max_count=24; keep_for=720h,max_count=30");
  });

  test("survives a round trip unchanged", () => {
    expect(textToBuckets(bucketsToText(policy))).toEqual(policy);
  });

  test("accepts newlines as well as semicolons", () => {
    const parsed = textToBuckets("keep_for=1h,max_count=-1\nkeep_for=24h,max_count=24");
    expect(parsed).toEqual([
      { keepFor: "1h", maxCount: -1 },
      { keepFor: "24h", maxCount: 24 },
    ]);
  });

  test("normalizes Go durations on the way out", () => {
    expect(bucketsToText([{ keepFor: "8760h0m0s", maxCount: 12 }])).toBe("keep_for=8760h,max_count=12");
  });

  test("ignores blank lines and stray whitespace", () => {
    const parsed = textToBuckets("  keep_for=1h , max_count=5 \n\n; \n keep_for=2h,max_count=1");
    expect(parsed).toEqual([
      { keepFor: "1h", maxCount: 5 },
      { keepFor: "2h", maxCount: 1 },
    ]);
  });

  test("keeps unlimited as -1 rather than dropping it", () => {
    // -1 is how the server expresses "keep everything in this window"; losing it
    // would silently start pruning recent versions.
    expect(textToBuckets("keep_for=1h,max_count=-1")[0]!.maxCount).toBe(-1);
  });
});

describe("incomplete rules", () => {
  test("an empty window stays empty instead of becoming 0s", () => {
    // "0s" would be a valid zero-length window, so an unfilled field must not
    // serialize into one; the editor refuses to save it instead.
    expect(tidyDuration("")).toBe("");
    expect(tidyDuration("   ")).toBe("");
  });

  test("a missing max_count reads as unlimited", () => {
    expect(textToBuckets("keep_for=1h")[0]!.maxCount).toBe(-1);
  });
});
