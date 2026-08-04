type ToastKind = "success" | "error";

function region(): HTMLElement {
  const existing = document.querySelector<HTMLElement>(".toast-region");
  if (existing) return existing;

  const created = document.createElement("div");
  created.className = "toast-region";
  created.setAttribute("aria-live", "polite");
  created.setAttribute("aria-atomic", "true");
  document.body.appendChild(created);
  return created;
}

function dismiss(element: Element): void {
  element.remove();
}

function arm(element: HTMLElement): void {
  const timeout = Number(element.dataset.toastTimeout);
  if (timeout > 0) window.setTimeout(() => dismiss(element), timeout);
}

/** Show feedback for a browser-side action without pulling in a UI framework. */
export function toast(message: string, kind: ToastKind = "success"): void {
  const element = document.createElement("div");
  element.className = `toast toast-${kind}`;
  element.dataset.toast = "";
  element.setAttribute("role", kind === "error" ? "alert" : "status");
  if (kind === "success") element.dataset.toastTimeout = "5000";

  const icon = document.createElement("i");
  icon.className = `ti ti-${kind === "success" ? "circle-check" : "alert-triangle"}`;
  icon.setAttribute("aria-hidden", "true");

  const text = document.createElement("span");
  text.textContent = message;

  const close = document.createElement("button");
  close.type = "button";
  close.className = "toast-close";
  close.dataset.toastClose = "";
  close.setAttribute("aria-label", "Dismiss notification");
  close.innerHTML = '<i class="ti ti-x" aria-hidden="true"></i>';

  element.append(icon, text, close);
  region().appendChild(element);
  arm(element);
}

document.addEventListener("click", (event) => {
  const target = event.target;
  if (!(target instanceof Element)) return;
  const close = target.closest("[data-toast-close]");
  if (close) dismiss(close.closest("[data-toast]") ?? close);
});

for (const element of document.querySelectorAll<HTMLElement>("[data-toast]")) arm(element);
