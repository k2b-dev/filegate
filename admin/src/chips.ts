/**
 * Editor for list-valued settings.
 *
 * A comma-separated text field made it easy to lose an entry to a stray comma
 * and gave no feedback until the value was already saved. Chips make each entry
 * a discrete thing that can be removed on its own, and the editor validates
 * against the server before closing, so an invalid combination is corrected
 * with the input still on screen.
 *
 * Covers the CORS lists, trusted proxies and the other string lists -- six keys
 * that all behaved the same badly.
 */
export type ChipsResult = string[] | null;

const hints: Record<string, string> = {
  "server.cors.allowed_origins": "Full origins including the scheme, such as https://admin.example.com. Use * to allow any, which cannot be combined with credentials.",
  "server.trusted_proxies": "IP addresses or CIDR ranges of proxies whose forwarded headers are honoured. Anything else can spoof its own address.",
  "server.cors.allowed_methods": "Leave empty to use the REST defaults.",
  "server.cors.allowed_headers": "Leave empty to use the REST defaults.",
  "server.cors.exposed_headers": "Headers the browser may read from a cross-origin response.",
};

function must(root: ParentNode, selector: string): HTMLElement {
  const el = root.querySelector(selector);
  if (!(el instanceof HTMLElement)) throw new Error(`missing ${selector}`);
  return el;
}

export function openChipsEditor(opts: { path: string; current: string[]; usage: string }): Promise<ChipsResult> {
  return new Promise((resolve) => {
    let entries = [...opts.current];

    const dialog = document.createElement("dialog");
    dialog.className = "prompt";
    dialog.innerHTML = `
      <div class="prompt-panel">
        <div class="prompt-head">
          <h2>Change setting</h2>
          <button type="button" class="prompt-close" data-cancel aria-label="Close dialog"><i class="ti ti-x" aria-hidden="true"></i></button>
        </div>
        <div class="prompt-body">
          <div class="prompt-message">
            <span class="prompt-badge"></span>
            <span class="prompt-message-text"></span>
          </div>
          <div class="chips" data-chips></div>
          <div class="field">
            <label for="chip-input">Add entry</label>
            <div class="chip-add">
              <input id="chip-input" class="input" data-chip-input placeholder="type and press Enter" />
              <button type="button" class="btn" data-chip-add><i class="ti ti-plus" aria-hidden="true"></i><span class="btn-label">Add</span></button>
            </div>
          </div>
          <div class="error" data-failure hidden></div>
        </div>
        <div class="prompt-footer">
          <button type="button" class="btn" data-cancel><i class="ti ti-x" aria-hidden="true"></i><span class="btn-label">Cancel</span></button>
          <button type="button" class="btn primary" data-save><i class="ti ti-check" aria-hidden="true"></i><span class="btn-label">Apply</span></button>
        </div>
      </div>`;
    document.body.appendChild(dialog);

    must(dialog, ".prompt-badge").textContent = opts.path;
    must(dialog, ".prompt-message-text").textContent = hints[opts.path] ?? opts.usage;

    const list = must(dialog, "[data-chips]");
    const input = must(dialog, "[data-chip-input]") as HTMLInputElement;
    const failure = must(dialog, "[data-failure]");
    const save = must(dialog, "[data-save]") as HTMLButtonElement;

    const render = () => {
      list.innerHTML = "";
      if (entries.length === 0) {
        const empty = document.createElement("span");
        empty.className = "muted";
        empty.textContent = "Empty — the server default applies.";
        list.appendChild(empty);
        return;
      }
      entries.forEach((entry, index) => {
        const chip = document.createElement("span");
        chip.className = "chip";
        chip.innerHTML = `<span class="chip-text"></span><button type="button" class="chip-remove" aria-label="Remove"><i class="ti ti-x" aria-hidden="true"></i></button>`;
        must(chip, ".chip-text").textContent = entry;
        must(chip, ".chip-remove").addEventListener("click", () => {
          entries.splice(index, 1);
          render();
        });
        list.appendChild(chip);
      });
    };

    const add = () => {
      // Splitting on commas too, so pasting an existing comma-separated value
      // does the obvious thing instead of creating one long entry.
      const added = input.value
        .split(",")
        .map((part) => part.trim())
        .filter((part) => part && !entries.includes(part));
      if (added.length === 0) return;
      entries.push(...added);
      input.value = "";
      failure.hidden = true;
      render();
    };

    must(dialog, "[data-chip-add]").addEventListener("click", add);
    input.addEventListener("keydown", (event) => {
      if ((event as KeyboardEvent).key !== "Enter") return;
      event.preventDefault();
      add();
    });

    const close = (value: ChipsResult) => {
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

    save.addEventListener("click", async () => {
      // Anything typed but not yet added would otherwise be silently dropped.
      add();
      save.disabled = true;
      failure.hidden = true;
      try {
        // Ask the server first. Cross-field rules live there -- credentials
        // cannot combine with a wildcard origin -- and finding that out after a
        // redirect means retyping the whole list.
        const res = await fetch("/api/config/validate", {
          method: "POST",
          headers: { "content-type": "application/json" },
          credentials: "same-origin",
          body: JSON.stringify({ path: opts.path, value: entries }),
        });
        if (!res.ok) {
          const body = await res.json().catch(() => ({}));
          failure.textContent = body.error ?? `Validation failed with ${res.status}`;
          failure.hidden = false;
          return;
        }
      } catch {
        failure.textContent = "Could not reach the server to validate this change.";
        failure.hidden = false;
        return;
      } finally {
        save.disabled = false;
      }
      close(entries);
    });

    render();
    document.documentElement.classList.add("has-prompt");
    dialog.showModal();
    input.focus();
  });
}
