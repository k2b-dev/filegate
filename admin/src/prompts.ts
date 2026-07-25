type PromptVariant = "default" | "danger";

type ConfirmOptions = {
  title: string;
  message: string;
  badge?: string;
  confirmText?: string;
  cancelText?: string;
  variant?: PromptVariant;
};

type Field = {
  name: string;
  label: string;
  value?: string;
  placeholder?: string;
  required?: boolean;
  type?: "text" | "number";
  options?: { value: string; label: string }[];
};

type FormOptions = {
  title: string;
  message?: string;
  badge?: string;
  confirmText?: string;
  cancelText?: string;
  fields: Field[];
};

function ensureDialog(): HTMLDialogElement {
  let dialog = document.querySelector<HTMLDialogElement>("#fg-prompt");
  if (dialog) return dialog;
  dialog = document.createElement("dialog");
  dialog.id = "fg-prompt";
  dialog.className = "prompt";
  document.body.appendChild(dialog);
  return dialog;
}

function button(text: string, value: string, variant?: PromptVariant): HTMLButtonElement {
  const el = document.createElement("button");
  el.type = "button";
  el.className = `btn${variant === "danger" ? " danger" : value === "ok" ? " primary" : ""}`;
  el.dataset.promptValue = value;
  el.textContent = text;
  return el;
}

function frame(title: string, message?: string, badge?: string): { root: HTMLDivElement; body: HTMLDivElement; footer: HTMLDivElement } {
  const root = document.createElement("div");
  root.className = "prompt-panel";

  const head = document.createElement("div");
  head.className = "prompt-head";
  const heading = document.createElement("h2");
  heading.textContent = title;
  const close = button("×", "cancel");
  close.className = "prompt-close";
  close.setAttribute("aria-label", "Close dialog");
  head.append(heading, close);

  const body = document.createElement("div");
  body.className = "prompt-body";
  if (message || badge) {
    const text = document.createElement("div");
    text.className = "prompt-message";
    if (badge) {
      const badgeEl = document.createElement("span");
      badgeEl.className = "prompt-badge";
      badgeEl.textContent = badge;
      text.append(badgeEl);
    }
    if (message) {
      const messageEl = document.createElement("span");
      messageEl.className = "prompt-message-text";
      messageEl.textContent = message;
      text.append(messageEl);
    }
    body.append(text);
  }

  const footer = document.createElement("div");
  footer.className = "prompt-footer";
  root.append(head, body, footer);
  return { root, body, footer };
}

function openPrompt<T>(root: HTMLElement, resolveValue: (value: string | undefined) => T, canClose?: (value: string | undefined) => boolean): Promise<T> {
  const dialog = ensureDialog();
  dialog.replaceChildren(root);

  let mouseDownOnBackdrop = false;
  return new Promise((resolve) => {
    const close = (value?: string) => {
      if (canClose && !canClose(value)) return;
      dialog.close();
      dialog.replaceChildren();
      document.documentElement.classList.remove("has-prompt");
      resolve(resolveValue(value));
    };

    dialog.oncancel = (event) => {
      event.preventDefault();
      close(undefined);
    };
    dialog.onmousedown = (event) => {
      mouseDownOnBackdrop = event.target === dialog;
    };
    dialog.onclick = (event) => {
      const target = event.target;
      if (target === dialog && mouseDownOnBackdrop) close(undefined);
      mouseDownOnBackdrop = false;
      if (!(target instanceof HTMLElement)) return;
      const action = target.closest<HTMLElement>("[data-prompt-value]");
      if (action) close(action.dataset.promptValue);
    };

    dialog.showModal();
    document.documentElement.classList.add("has-prompt");
    window.requestAnimationFrame(() => dialog.querySelector<HTMLElement>("input, select, button")?.focus());
  });
}

const prompts = {
  confirm(options: ConfirmOptions): Promise<boolean> {
    const { root, footer } = frame(options.title, options.message, options.badge);
    footer.append(button(options.cancelText ?? "Cancel", "cancel"), button(options.confirmText ?? "Confirm", "ok", options.variant));
    return openPrompt(root, (value) => value === "ok");
  },

  form(options: FormOptions): Promise<Record<string, string> | undefined> {
    const { root, body, footer } = frame(options.title, options.message, options.badge);
    const form = document.createElement("form");
    form.className = "prompt-form";

    for (const field of options.fields) {
      const label = document.createElement("label");
      label.className = "field";
      const text = document.createElement("span");
      text.textContent = field.label;
      label.append(text);

      const input = field.options ? document.createElement("select") : document.createElement("input");
      input.className = field.options ? "select" : "input";
      input.name = field.name;
      input.required = field.required ?? false;
      if (!field.options && input instanceof HTMLInputElement) input.type = field.type ?? "text";
      if (field.placeholder && input instanceof HTMLInputElement) input.placeholder = field.placeholder;
      if (field.options && input instanceof HTMLSelectElement) {
        for (const option of field.options) {
          const el = document.createElement("option");
          el.value = option.value;
          el.textContent = option.label;
          input.append(el);
        }
      }
      if (field.value) input.value = field.value;
      label.append(input);
      form.append(label);
    }

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      const submit = root.querySelector<HTMLButtonElement>("[data-prompt-value='ok']");
      submit?.click();
    });

    body.append(form);
    footer.append(button(options.cancelText ?? "Cancel", "cancel"), button(options.confirmText ?? "Apply", "ok"));

    return openPrompt(root, (value) => {
      if (value !== "ok") return undefined;
      return Object.fromEntries(new FormData(form).entries()) as Record<string, string>;
    }, (value) => value !== "ok" || form.reportValidity());
  },
};

/** The folder currently being viewed, taken from the URL. */
function currentFolderPath(): string {
  return new URLSearchParams(location.search).get("path") ?? "";
}

function submitForm(action: string, values: Record<string, string>) {
  const form = document.createElement("form");
  form.method = "post";
  form.action = action;
  form.hidden = true;
  for (const [name, value] of Object.entries(values)) {
    const input = document.createElement("input");
    input.type = "hidden";
    input.name = name;
    input.value = value;
    form.append(input);
  }
  document.body.append(form);
  form.requestSubmit();
}

document.addEventListener("submit", async (event) => {
  const form = event.target;
  if (!(form instanceof HTMLFormElement) || !form.matches("[data-confirm-delete]")) return;
  event.preventDefault();
  const label = form.dataset.confirmDelete || "this resource";
  const confirmed = await prompts.confirm({
    title: "Delete resource",
    badge: label,
    message: "This action permanently removes the selected resource and its contents.",
    confirmText: "Delete",
    variant: "danger",
  });
  if (confirmed) form.submit();
});

document.addEventListener("submit", async (event) => {
  const form = event.target;
  if (!(form instanceof HTMLFormElement) || !form.matches("[data-confirm-rescan]")) return;
  event.preventDefault();
  const confirmed = await prompts.confirm({
    title: "Rescan index",
    message: "Start a filesystem index rescan in the background. Activity will show the event after the server finishes the request.",
    confirmText: "Start rescan",
  });
  if (confirmed) form.submit();
});

document.addEventListener("click", async (event) => {
  const target = event.target;
  if (!(target instanceof Element)) return;
  const mkdir = target.closest<HTMLElement>("[data-mkdir-open]");
  if (mkdir) {
    const values = await prompts.form({
      title: "Create folder",
      badge: mkdir.dataset.mkdirParent || "/",
      message: "Create a folder inside the current location. Conflict behavior controls what happens when the folder name is already in use.",
      confirmText: "Create folder",
      fields: [
        { name: "name", label: "Folder name", placeholder: "New folder", required: true },
        {
          name: "onConflict",
          label: "On conflict",
          value: "error",
          options: [
            { value: "error", label: "Error" },
            { value: "skip", label: "Use existing folder" },
            { value: "rename", label: "Create with next free name" },
          ],
        },
      ],
    });
    if (values) submitForm("/files/mkdir", { parentPath: mkdir.dataset.mkdirParent || "", ...values });
    return;
  }

  const rename = target.closest<HTMLElement>("[data-rename-open]");
  if (rename) {
    const values = await prompts.form({
      title: "Rename",
      badge: rename.dataset.renamePath,
      message: "Use a unique name within the current folder. The update keeps the resource ID and existing metadata in place.",
      confirmText: "Rename",
      fields: [{ name: "name", label: "New name", value: rename.dataset.renameName || "", required: true }],
    });
    if (values) submitForm("/files/rename", { id: rename.dataset.renameId || "", parentPath: currentFolderPath(), name: values.name || "" });
    return;
  }

  const metadata = target.closest<HTMLElement>("[data-metadata-open]");
  if (metadata) {
    const isDirectory = metadata.dataset.metadataKind === "directory";
    const values = await prompts.form({
      title: "Edit metadata",
      badge: metadata.dataset.metadataPath,
      message: `UID and GID must be set together. Modes are octal strings, for example 644 for files or 755 for directories.${
        isDirectory ? "\nRecursive updates apply ownership and modes to every descendant." : ""
      }`,
      confirmText: "Save metadata",
      fields: [
        { name: "uid", label: "Owner UID", value: metadata.dataset.metadataUid || "", type: "number", required: true },
        { name: "gid", label: "Group GID", value: metadata.dataset.metadataGid || "", type: "number", required: true },
        { name: "mode", label: "Mode", value: metadata.dataset.metadataMode || "", placeholder: "644", required: true },
        ...(isDirectory
          ? [
              { name: "dirMode", label: "Directory mode", value: metadata.dataset.metadataMode || "", placeholder: "755" },
              {
                name: "recursiveOwnership",
                label: "Apply recursively",
                value: "false",
                options: [
                  { value: "false", label: "No, only this folder" },
                  { value: "true", label: "Yes, include descendants" },
                ],
              },
            ]
          : []),
      ],
    });
    if (values) submitForm("/files/metadata", { id: metadata.dataset.metadataId || "", parentPath: currentFolderPath(), ...values });
    return;
  }

  const trigger = target.closest<HTMLElement>("[data-transfer-open]");
  if (!trigger) return;

  const values = await prompts.form({
    title: "Move or copy",
    badge: trigger.dataset.transferPath,
    message: "Choose an existing target folder and the final resource name. The target must be a folder inside a mount, such as files or files/archive. Conflict behavior controls the final target: Error keeps the current state, Rename writes to the next free sibling name, Overwrite replaces the target.",
    confirmText: "Apply transfer",
    fields: [
      {
        name: "op",
        label: "Operation",
        value: "move",
        options: [
          { value: "move", label: "Move" },
          { value: "copy", label: "Copy" },
        ],
      },
      {
        name: "onConflict",
        label: "On conflict",
        value: "error",
        options: [
          { value: "error", label: "Error" },
          { value: "rename", label: "Rename" },
          { value: "overwrite", label: "Overwrite" },
        ],
      },
      {
        name: "targetParentPath",
        label: "Target parent path",
        placeholder: "backups/archive",
        value: trigger.dataset.transferParent || "",
        required: true,
      },
      { name: "targetName", label: "Target name", value: trigger.dataset.transferName || "", required: true },
    ],
  });
  if (!values) return;
  submitForm("/files/transfer", { id: trigger.dataset.transferId || "", parentPath: currentFolderPath(), ...values });
});

document.documentElement.dataset.promptsReady = "true";
