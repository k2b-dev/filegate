import { copyToClipboard } from "@valentinkolb/stdlib/browser";

/**
 * One-time display of a freshly issued credential.
 *
 * Shown in a dialog rather than carried through a redirect: a secret in a query
 * string ends up in the browser history and in every access log between here
 * and the server. It cannot be retrieved again afterwards, so the dialog says
 * so and offers a copy button instead of asking anyone to select 40 characters
 * by hand.
 */
export function showCredential(opts: { title: string; accessKey: string; secretKey: string; note?: string }): void {
  const dialog = document.createElement("dialog");
  dialog.className = "prompt";
  dialog.innerHTML = `
    <div class="prompt-panel">
      <div class="prompt-head">
        <h2></h2>
      </div>
      <div class="prompt-body">
        <div class="prompt-message">
          <span class="prompt-message-text">This secret is shown once and cannot be recovered. Store it now; if you lose it, rotate the key to get a new one.</span>
        </div>
        <div class="cred-field">
          <span class="muted">Access key</span>
          <div class="cred-row">
            <code data-access></code>
            <button type="button" class="btn cred-copy" data-copy-access><i class="ti ti-copy" aria-hidden="true"></i><span class="btn-label">Copy</span></button>
          </div>
        </div>
        <div class="cred-field">
          <span class="muted">Secret key</span>
          <div class="cred-row">
            <code data-secret></code>
            <button type="button" class="btn cred-copy" data-copy-secret><i class="ti ti-copy" aria-hidden="true"></i><span class="btn-label">Copy</span></button>
          </div>
        </div>
        <p class="muted hint" data-note hidden></p>
      </div>
      <div class="prompt-footer">
        <button type="button" class="btn primary" data-done><i class="ti ti-check" aria-hidden="true"></i><span class="btn-label">I have stored it</span></button>
      </div>
    </div>`;
  document.body.appendChild(dialog);

  const pick = (selector: string) => dialog.querySelector(selector) as HTMLElement;
  pick("h2").textContent = opts.title;
  pick("[data-access]").textContent = opts.accessKey;
  pick("[data-secret]").textContent = opts.secretKey;
  if (opts.note) {
    const note = pick("[data-note]");
    note.textContent = opts.note;
    note.hidden = false;
  }

  const wire = (selector: string, value: string) => {
    const button = pick(selector) as HTMLButtonElement;
    const icon = button.querySelector("i");
    const label = button.querySelector(".btn-label");
    button.addEventListener("click", () => {
      void copyToClipboard(value).then(
        () => {
          if (icon) icon.className = "ti ti-check";
          if (label) label.textContent = "Copied";
          window.setTimeout(() => {
            if (icon) icon.className = "ti ti-copy";
            if (label) label.textContent = "Copy";
          }, 1600);
        },
        // Clipboard access can be denied; say so rather than pretending it worked.
        () => {
          if (icon) icon.className = "ti ti-alert-triangle";
          if (label) label.textContent = "Copy failed";
        },
      );
    });
  };
  wire("[data-copy-access]", opts.accessKey);
  wire("[data-copy-secret]", opts.secretKey);

  const close = () => {
    dialog.close();
    dialog.remove();
    document.documentElement.classList.remove("has-prompt");
    // The listing must show the new key, and the page was rendered before it existed.
    location.assign("/settings");
  };
  pick("[data-done]").addEventListener("click", close);
  dialog.addEventListener("cancel", (event) => {
    event.preventDefault();
    close();
  });

  document.documentElement.classList.add("has-prompt");
  dialog.showModal();
}
