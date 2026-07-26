import { prompts, submitForm } from "./prompts";

/**
 * Settings page interactions.
 *
 * Editing is typed from the schema: a bool gets a select, a duration and an int
 * get text fields with the shape named in the placeholder. The server validates
 * regardless, so this only shortens the feedback loop.
 */
document.addEventListener("submit", async (event) => {
  const form = event.target;
  if (!(form instanceof HTMLFormElement) || !form.matches("[data-confirm-s3key]")) return;
  event.preventDefault();
  const confirmed = await prompts.confirm({
    title: "Delete access key",
    badge: form.dataset.confirmS3key,
    message: "The key stops working immediately and stays deleted across restarts, even if it was originally seeded from configuration.",
    confirmText: "Delete key",
    variant: "danger",
  });
  if (confirmed) form.submit();
});

document.addEventListener("click", async (event) => {
  const target = event.target;
  if (!(target instanceof Element)) return;

  const edit = target.closest<HTMLElement>("[data-setting-edit]");
  if (edit) {
    const path = edit.dataset.settingEdit ?? "";
    const type = edit.dataset.settingType ?? "string";
    const current = edit.dataset.settingValue ?? "";
    const values = await prompts.form({
      title: "Change setting",
      badge: path,
      message: hintFor(type),
      confirmText: "Apply",
      fields: [
        type === "bool"
          ? { name: "value", label: "Value", value: current === "true" ? "true" : "false", options: [{ value: "true", label: "true" }, { value: "false", label: "false" }] }
          : { name: "value", label: "Value", value: current === "-" ? "" : current, placeholder: placeholderFor(type), required: true },
      ],
    });
    if (values) submitForm("/settings/apply", { path, type, value: values.value ?? "" });
    return;
  }

  const rotate = target.closest<HTMLElement>("[data-s3key-rotate]");
  if (rotate) {
    const accessKey = rotate.dataset.s3keyRotate ?? "";
    const confirmed = await prompts.confirm({
      title: "Rotate secret",
      badge: accessKey,
      message: "Issues a new secret and invalidates the current one immediately. Anything still using the old secret stops working.",
      confirmText: "Rotate",
    });
    if (confirmed) submitForm("/settings/s3keys/rotate", { accessKey });
    return;
  }

  if (target.closest("[data-s3key-create]")) {
    const values = await prompts.form({
      title: "Create access key",
      message: "The secret is shown once after creation and cannot be retrieved later. Use * to grant every mount.",
      confirmText: "Create key",
      fields: [
        { name: "buckets", label: "Buckets", placeholder: "files, photos or *", required: true },
        { name: "requestsPerSecond", label: "Requests per second", placeholder: "leave empty for no limit" },
      ],
    });
    if (values) submitForm("/settings/s3keys/create", { buckets: values.buckets ?? "", requestsPerSecond: values.requestsPerSecond ?? "" });
  }
});

function hintFor(type: string): string {
  if (type === "duration") return "A Go duration such as 30s, 5m or 24h.";
  if (type === "stringList") return "Comma separated. Leave empty for none.";
  if (type === "int") return "A whole number. Byte limits are in bytes.";
  return "The server validates the value before applying it.";
}

function placeholderFor(type: string): string {
  if (type === "duration") return "5m";
  if (type === "stringList") return "one, two";
  if (type === "int") return "1048576";
  return "";
}
