/**
 * Structured editor for the version retention policy.
 *
 * A raw text field was the first attempt and it is the wrong shape for this:
 * the policy is a list of (window, count) rules, and reading it back from
 * "keep_for=720h0m0s,max_count=30" is work the interface should be doing. The
 * raw mode stays available because an operator copying a policy between
 * installs wants text, and because a structured form should never be the only
 * way to express something.
 */
export type Bucket = { keepFor: string; maxCount: number };

/** Windows an operator reaches for, so the common case is one click. */
const presets: { label: string; value: string }[] = [
  { label: "1 hour", value: "1h" },
  { label: "1 day", value: "24h" },
  { label: "1 week", value: "168h" },
  { label: "30 days", value: "720h" },
  { label: "1 year", value: "8760h" },
];

/** Go duration strings arrive as 720h0m0s; show the part that carries meaning. */
export function tidyDuration(raw: string): string {
  const trimmed = raw.trim();
  // An empty window stays empty. Turning it into "0s" would let an unfilled
  // rule serialize as a zero-length window instead of being rejected.
  if (!trimmed) return "";
  const match = /^(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)s)?$/.exec(trimmed);
  if (!match) return trimmed;
  const [, h, m, sec] = match;
  const parts = [
    h && h !== "0" ? `${h}h` : "",
    m && m !== "0" ? `${m}m` : "",
    sec && sec !== "0" ? `${sec}s` : "",
  ].filter(Boolean);
  return parts.length ? parts.join("") : "0s";
}

export function bucketsToText(buckets: Bucket[]): string {
  return buckets.map((bucket) => `keep_for=${tidyDuration(bucket.keepFor)},max_count=${bucket.maxCount}`).join("; ");
}

export function textToBuckets(raw: string): Bucket[] {
  return raw
    .split(/[\n;]/)
    .map((line) => line.trim())
    .filter(Boolean)
    .map((rule) => {
      const fields = new Map(rule.split(",").map((part) => part.split("=").map((s) => s.trim()) as [string, string]));
      return { keepFor: fields.get("keep_for") ?? "", maxCount: Number(fields.get("max_count") ?? -1) };
    });
}

function must(root: ParentNode, selector: string): HTMLElement {
  const el = root.querySelector(selector);
  if (!(el instanceof HTMLElement)) throw new Error(`missing ${selector}`);
  return el;
}

/**
 * Opens the editor and resolves with the policy in the CLI's text syntax, or
 * null when cancelled. The caller submits it unchanged, so both modes go
 * through the same server-side parser.
 */
export function openRetentionEditor(initial: Bucket[]): Promise<string | null> {
  return new Promise((resolve) => {
    let rules: Bucket[] = initial.length
      ? initial.map((bucket) => ({ keepFor: tidyDuration(bucket.keepFor), maxCount: bucket.maxCount }))
      : [{ keepFor: "24h", maxCount: 24 }];
    let raw = false;

    const dialog = document.createElement("dialog");
    dialog.className = "prompt";
    dialog.innerHTML = `
      <div class="prompt-panel">
        <div class="prompt-head">
          <h2>Version retention</h2>
          <button type="button" class="prompt-close" data-cancel aria-label="Close dialog">&times;</button>
        </div>
        <div class="prompt-body">
          <div class="prompt-message">
            <span class="prompt-message-text">Each rule is a window measured back from now. Ordering them shortest to longest forms a decay schedule: keep everything recent, thin out as versions age.</span>
          </div>
          <div data-structured>
            <div class="retention-rules" data-rules></div>
            <button type="button" class="btn retention-add" data-add><i class="ti ti-plus" aria-hidden="true"></i><span class="btn-label">Add rule</span></button>
            <p class="muted hint" data-summary></p>
          </div>
          <div data-raw hidden>
            <div class="field">
              <label for="retention-raw">Rules</label>
              <textarea id="retention-raw" class="input retention-raw" rows="5" spellcheck="false"></textarea>
              <span class="muted">One rule per line: keep_for=&lt;duration&gt;,max_count=&lt;n&gt;. Use -1 to keep everything in that window.</span>
            </div>
          </div>
        </div>
        <div class="prompt-footer retention-footer">
          <button type="button" class="btn" data-toggle-raw><i class="ti ti-code" aria-hidden="true"></i><span class="btn-label">Edit as text</span></button>
          <span class="tb-group">
            <button type="button" class="btn" data-cancel><i class="ti ti-x" aria-hidden="true"></i><span class="btn-label">Cancel</span></button>
            <button type="button" class="btn primary" data-save><i class="ti ti-check" aria-hidden="true"></i><span class="btn-label">Save policy</span></button>
          </span>
        </div>
      </div>`;
    document.body.appendChild(dialog);

    const list = must(dialog, "[data-rules]");
    const summary = must(dialog, "[data-summary]");
    const structured = must(dialog, "[data-structured]");
    const rawPane = must(dialog, "[data-raw]");
    const rawInput = must(dialog, "#retention-raw") as HTMLTextAreaElement;
    const toggle = must(dialog, "[data-toggle-raw]");

    const describe = () =>
      rules
        .map((rule) => (rule.maxCount < 0 ? `keep all within ${rule.keepFor || "?"}` : `keep ${rule.maxCount} within ${rule.keepFor || "?"}`))
        .join(" · ");

    const render = () => {
      list.innerHTML = "";
      rules.forEach((rule, index) => {
        const row = document.createElement("div");
        row.className = "retention-rule";
        row.innerHTML = `
          <label class="rr-field">
            <span class="muted">Within</span>
            <input class="input rr-window" list="retention-presets" value="" placeholder="24h" aria-label="Window for rule ${index + 1}" />
          </label>
          <label class="rr-field">
            <span class="muted">Keep</span>
            <input class="input rr-count" type="number" min="0" value="" aria-label="Versions to keep for rule ${index + 1}" />
          </label>
          <label class="rr-all">
            <input type="checkbox" class="rr-keep-all" /> <span>all</span>
          </label>
          <button type="button" class="btn danger rr-remove" aria-label="Remove rule ${index + 1}"><i class="ti ti-trash" aria-hidden="true"></i></button>`;

        const window = must(row, ".rr-window") as HTMLInputElement;
        const count = must(row, ".rr-count") as HTMLInputElement;
        const keepAll = must(row, ".rr-keep-all") as HTMLInputElement;

        window.value = rule.keepFor;
        keepAll.checked = rule.maxCount < 0;
        count.value = rule.maxCount < 0 ? "" : String(rule.maxCount);
        count.disabled = keepAll.checked;

        window.addEventListener("input", () => {
          rules[index]!.keepFor = window.value.trim();
          summary.textContent = describe();
        });
        count.addEventListener("input", () => {
          rules[index]!.maxCount = Number(count.value || 0);
          summary.textContent = describe();
        });
        // -1 is how the server expresses "unlimited"; a checkbox says it better.
        keepAll.addEventListener("change", () => {
          count.disabled = keepAll.checked;
          rules[index]!.maxCount = keepAll.checked ? -1 : Number(count.value || 0);
          summary.textContent = describe();
        });
        must(row, ".rr-remove").addEventListener("click", () => {
          rules.splice(index, 1);
          if (rules.length === 0) rules.push({ keepFor: "24h", maxCount: 24 });
          render();
        });

        list.appendChild(row);
      });
      summary.textContent = describe();
    };

    const datalist = document.createElement("datalist");
    datalist.id = "retention-presets";
    for (const preset of presets) {
      const option = document.createElement("option");
      option.value = preset.value;
      option.label = preset.label;
      datalist.appendChild(option);
    }
    dialog.appendChild(datalist);

    must(dialog, "[data-add]").addEventListener("click", () => {
      rules.push({ keepFor: "", maxCount: 10 });
      render();
    });

    toggle.addEventListener("click", () => {
      if (!raw) {
        // Carry the current rules into the text view so switching never loses work.
        rawInput.value = rules.map((rule) => `keep_for=${rule.keepFor},max_count=${rule.maxCount}`).join("\n");
      } else {
        rules = textToBuckets(rawInput.value);
        if (rules.length === 0) rules = [{ keepFor: "24h", maxCount: 24 }];
        render();
      }
      raw = !raw;
      structured.hidden = raw;
      rawPane.hidden = !raw;
      const toggleLabel = toggle.querySelector('.btn-label');
      if (toggleLabel) toggleLabel.textContent = raw ? "Edit as fields" : "Edit as text";
    });

    const close = (value: string | null) => {
      dialog.close();
      dialog.remove();
      document.documentElement.classList.remove("has-prompt");
      resolve(value);
    };

    for (const button of dialog.querySelectorAll("[data-cancel]")) button.addEventListener("click", () => close(null));
    dialog.addEventListener("cancel", (event) => {
      event.preventDefault();
      close(null);
    });
    const failure = document.createElement("div");
    failure.className = "error";
    failure.hidden = true;
    must(dialog, ".prompt-body").appendChild(failure);

    must(dialog, "[data-save]").addEventListener("click", () => {
      const current = raw ? textToBuckets(rawInput.value) : rules;
      const incomplete = current.filter((rule) => !tidyDuration(rule.keepFor));
      if (current.length === 0 || incomplete.length > 0) {
        failure.hidden = false;
        failure.textContent =
          current.length === 0
            ? "Add at least one rule, or cancel and use Reset to restore the default policy."
            : `${incomplete.length} rule${incomplete.length === 1 ? "" : "s"} have no window. Fill it in, for example 24h.`;
        return;
      }
      close(bucketsToText(current));
    });

    render();
    document.documentElement.classList.add("has-prompt");
    dialog.showModal();
  });
}
