import { text } from "@valentinkolb/stdlib";

export function formatBytes(value: number): string {
  if (!value) return "-";
  return text.pprintBytes(value);
}

export function formatRetryAfter(seconds: number): string | undefined {
  if (!Number.isFinite(seconds) || seconds <= 0) return undefined;
  return text.pprintDurationMs(seconds * 1000);
}

export function formatUnix(value: number): string {
  if (!value) return "-";
  const millis = value > 100_000_000_000 ? value : value * 1000;
  return new Date(millis).toISOString().slice(0, 16).replace("T", " ");
}

export function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return "Operation failed";
}

export function redirectFiles(path: string, error?: string, notice?: string): string {
  const q = new URLSearchParams();
  if (path.trim()) q.set("path", path);
  if (error) q.set("error", error);
  if (notice) q.set("notice", notice);
  const suffix = q.toString();
  return `/files${suffix ? `?${suffix}` : ""}`;
}

export function selectedFiles(path: string, id: string, error?: string, notice?: string): string {
  const q = new URLSearchParams({ path, id });
  if (error) q.set("error", error);
  if (notice) q.set("notice", notice);
  return `/files?${q}`;
}
