import { describe, expect, test } from "bun:test";
import { renderToString } from "solid-js/web";
import { Toasts } from "../src/components/Toasts";

describe("action feedback", () => {
  test("renders dismissible, expiring success feedback", () => {
    const html = renderToString(() => <Toasts notice="Index rescan started" />);

    expect(html).toContain('role="status"');
    expect(html).toContain('data-toast-timeout="5000"');
    expect(html).toContain("Index rescan started");
    expect(html).toContain('aria-label="Dismiss notification"');
  });

  test("keeps errors visible until they are dismissed", () => {
    const html = renderToString(() => <Toasts error="Prune failed" />);

    expect(html).toContain('role="alert"');
    expect(html).toContain("Prune failed");
    expect(html).not.toContain("data-toast-timeout");
  });

  test("hidden controls stay hidden when a component declares its own display", async () => {
    const css = await Bun.file(new URL("../src/styles.css", import.meta.url)).text();

    expect(css).toContain("[hidden]{display:none!important}");
  });
});
