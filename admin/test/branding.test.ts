import { describe, expect, test } from "bun:test";
import { defaultAdminName, resolveAdminName } from "../src/lib/branding";

describe("admin branding", () => {
  test("uses the product name by default", () => {
    expect(resolveAdminName(undefined)).toBe(defaultAdminName);
    expect(resolveAdminName("   ")).toBe(defaultAdminName);
  });

  test("uses a trimmed deployment name when configured", () => {
    expect(resolveAdminName("  fg-1-eu  ")).toBe("fg-1-eu");
  });
});
