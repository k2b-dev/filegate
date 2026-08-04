import { showCredential } from "./credential";
import { prompts } from "./prompts";

document.addEventListener("submit", async (event) => {
  const form = event.target;
  if (!(form instanceof HTMLFormElement) || !form.matches("[data-confirm-s3key]")) return;
  event.preventDefault();
  const confirmed = await prompts.confirm({
    title: "Delete access key",
    badge: form.dataset.confirmS3key,
    message: "The key stops working immediately and stays deleted across restarts.",
    confirmText: "Delete key",
    variant: "danger",
  });
  if (confirmed) form.submit();
});

document.addEventListener("click", async (event) => {
  const target = event.target;
  if (!(target instanceof Element)) return;

  const rotate = target.closest<HTMLElement>("[data-s3key-rotate]");
  if (rotate) {
    const accessKey = rotate.dataset.s3keyRotate ?? "";
    const confirmed = await prompts.confirm({
      title: "Rotate secret",
      badge: accessKey,
      message: "Issues a new secret and invalidates the current one immediately. Anything still using the old secret stops working.",
      confirmText: "Rotate",
    });
    if (!confirmed) return;
    const res = await fetch(`/api/s3keys/${encodeURIComponent(accessKey)}/rotate`, { method: "POST", credentials: "same-origin" });
    const body = await res.json();
    if (!res.ok) {
      await prompts.confirm({ title: "Could not rotate key", message: body.error ?? "Unknown error", confirmText: "Close" });
      return;
    }
    showCredential({ title: "Secret rotated", accessKey: body.accessKey, secretKey: body.secretKey, note: "The previous secret stopped working immediately." });
    return;
  }

  const create = target.closest<HTMLElement>("[data-s3key-create]");
  if (!create) return;

  const mounts = (create.dataset.mounts ?? "").split(",").filter(Boolean);
  const values = await prompts.form({
    title: "Create access key",
    message: "The server generates the access key and secret. The secret is shown once.",
    confirmText: "Create key",
    fields: [
      {
        name: "buckets",
        label: "Buckets",
        value: "*",
        options: [{ value: "*", label: "All mounts" }, ...mounts.map((mount) => ({ value: mount, label: mount }))],
      },
      { name: "requestsPerSecond", label: "Requests per second", placeholder: "leave empty for no limit" },
    ],
  });
  if (!values) return;

  const res = await fetch("/api/s3keys/create", {
    method: "POST",
    headers: { "content-type": "application/json" },
    credentials: "same-origin",
    body: JSON.stringify({ buckets: [values.buckets ?? "*"], requestsPerSecond: values.requestsPerSecond ?? "" }),
  });
  const body = await res.json();
  if (!res.ok) {
    await prompts.confirm({ title: "Could not create key", message: body.error ?? "Unknown error", confirmText: "Close" });
    return;
  }
  showCredential({
    title: "Access key created",
    accessKey: body.accessKey,
    secretKey: body.secretKey,
    note: `Buckets: ${(body.buckets ?? []).join(", ")}`,
  });
});
