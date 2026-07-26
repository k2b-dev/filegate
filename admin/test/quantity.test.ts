import { describe, expect, test } from "bun:test";
import { unitKindFor } from "../src/quantity";

describe("unit kind selection", () => {
  test("byte-valued keys get the byte editor regardless of their type", () => {
    // The type is "int" for both a byte limit and a max-count, so only the unit
    // can tell them apart.
    expect(unitKindFor("int", "bytes")).toBe("bytes");
    expect(unitKindFor("int", undefined)).toBeNull();
  });

  test("durations are recognised from the type", () => {
    expect(unitKindFor("duration", undefined)).toBe("duration");
  });

  test("everything else falls back to the plain field", () => {
    expect(unitKindFor("string", undefined)).toBeNull();
    expect(unitKindFor("bool", undefined)).toBeNull();
    expect(unitKindFor("stringList", undefined)).toBeNull();
    expect(unitKindFor("retentionBuckets", undefined)).toBeNull();
  });
});
