/**
 * Number-plus-unit editor for byte counts and durations.
 *
 * Typing 1073741824 by hand is an error waiting to happen, and the raw field
 * gave no indication which unit a number was even in. Together these two kinds
 * cover roughly a third of the configuration.
 */
export type UnitKind = "bytes" | "duration";

const byteUnits: [string, number][] = [
  ["B", 1],
  ["KiB", 1024],
  ["MiB", 1024 ** 2],
  ["GiB", 1024 ** 3],
  ["TiB", 1024 ** 4],
];

const durationUnits: [string, number][] = [
  ["s", 1],
  ["m", 60],
  ["h", 3600],
  ["d", 86400],
];

/** Returns the editor to use for a key, or null to fall back to the plain field. */
export function unitKindFor(type: string, unit?: string): UnitKind | null {
  if (unit === "bytes") return "bytes";
  if (type === "duration") return "duration";
  return null;
}

/** Splits a value into the largest unit that divides it exactly. */
function decompose(total: number, units: [string, number][]): { amount: number; unit: string } {
  for (let i = units.length - 1; i >= 0; i--) {
    const [name, factor] = units[i]!;
    if (total >= factor && total % factor === 0) return { amount: total / factor, unit: name };
  }
  return { amount: total, unit: units[0]![0] };
}

function parseDurationSeconds(raw: string): number {
  const match = /^(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)s)?$/.exec(raw.trim());
  if (!match) return 0;
  return Number(match[1] ?? 0) * 3600 + Number(match[2] ?? 0) * 60 + Number(match[3] ?? 0);
}

export async function openQuantityEditor(opts: { path: string; kind: UnitKind; current: string; hint: string }): Promise<string | null> {
  // Imported lazily: prompts registers document listeners at module load, which
  // makes this file unimportable without a DOM and its pure helpers untestable.
  const { prompts } = await import("./prompts");
  const units = opts.kind === "bytes" ? byteUnits : durationUnits;
  const total = opts.kind === "bytes" ? Number(opts.current || 0) : parseDurationSeconds(opts.current);
  const start = decompose(Number.isFinite(total) ? total : 0, units);

  const values = await prompts.form({
    title: "Change setting",
    badge: opts.path,
    message: opts.hint,
    confirmText: "Apply",
    fields: [
      { name: "amount", label: "Amount", value: String(start.amount), type: "number", required: true },
      { name: "unit", label: "Unit", value: start.unit, options: units.map(([name]) => ({ value: name, label: name })) },
    ],
  });
  if (!values) return null;

  const amount = Number(values.amount);
  if (!Number.isFinite(amount) || amount < 0) return null;
  const factor = units.find(([name]) => name === values.unit)?.[1] ?? 1;

  // Bytes go back as an integer; durations as the string form Go parses, which
  // is also what the config file uses.
  if (opts.kind === "bytes") return String(Math.round(amount * factor));
  return `${Math.round(amount * factor)}s`;
}
