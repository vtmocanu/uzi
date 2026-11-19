// Hosted worker sizes (PRD #58, made admin-editable by PRD #4213).
//
// Sizes are DATA now: an admin edits what each one buys on Admin → Workers, and the api
// ships every size's name and six resolved integers in GET /workers/hosted/config (and
// GET /admin/worker-sizes). So this file no longer mirrors a table. It holds:
//
//   * the display helpers that turn those integers into the human units the UI shows
//     (CPU cores, memory and disk quantities), so every surface spells a size alike;
//   * the builtin seed, used ONLY where a fallback is required: an older api whose config
//     carries no `sizes`, and the default-size settings writes, which the server accepts
//     for the builtin names only until admin-added sizes land.
//
// The builtin seed is pinned to api/internal/hostedsvc/testdata/hosted_sizes.json (the
// seed contract the api generates) by workerSizes.test.ts. Do not put a number in this
// file that does not come from that golden.

import type { HostedConfigSize, WorkerSizeSpec } from "./apiTypes";

/** A size name plus its resolved integers: the shape every size display takes. */
export type NamedWorkerSize = WorkerSizeSpec & { readonly name: string };

/**
 * The builtin sizes as the api seeds them, in display order (smallest first). A FALLBACK
 * only: the live list comes from the api, where an admin may have changed every number.
 */
export const BUILTIN_WORKER_SIZES: readonly NamedWorkerSize[] = [
  { name: "s", cpu_request_millicores: 250, cpu_limit_millicores: 1000, memory_request_mib: 2048, memory_limit_mib: 4096, data_gib: 5, dind_data_gib: 20 },
  { name: "m", cpu_request_millicores: 1000, cpu_limit_millicores: 4000, memory_request_mib: 8192, memory_limit_mib: 12288, data_gib: 25, dind_data_gib: 20 },
  { name: "l", cpu_request_millicores: 1000, cpu_limit_millicores: 4000, memory_request_mib: 14336, memory_limit_mib: 20480, data_gib: 50, dind_data_gib: 50 },
  { name: "l-highmem", cpu_request_millicores: 1000, cpu_limit_millicores: 4000, memory_request_mib: 24576, memory_limit_mib: 32768, data_gib: 50, dind_data_gib: 50 },
];

/** The builtin size names, which are the only names a default-size setting accepts for now. */
export const BUILTIN_WORKER_SIZE_NAMES: readonly string[] = BUILTIN_WORKER_SIZES.map((s) => s.name);

/**
 * New persistent hosted workers start at L (#2240) unless the admin chose another default
 * and it exists. Ephemeral provisioning defaults to M (#2412). Rationale: ADR-2240.
 */
export const DEFAULT_WORKER_SIZE = "l";

/**
 * The sizes a picker offers: the api's list when it sent one, else the builtin seed (an
 * older api that predates editable sizes, whose sizes are exactly the builtins).
 */
export function sizeChoices(sizes: readonly HostedConfigSize[] | null | undefined): readonly NamedWorkerSize[] {
  return sizes ?? BUILTIN_WORKER_SIZES;
}

/**
 * Display spelling of a size name. Upper-case is for READING only — the wire value is
 * lowercase and nothing upper-cased is ever a value. Keep this the only place the two diverge.
 */
export function sizeLabel(name: string): string {
  return name.toUpperCase();
}

// Integer division rendered exactly. Both divisors (1000, 1024) give a terminating decimal,
// so toString never rounds; trailing zeros never appear because Number drops them.
function exactRatio(n: number, d: number): string {
  return String(n / d);
}

/** CPU in cores, from millicores: 250 → "0.25", 1000 → "1", 1500 → "1.5". */
export function cpuCores(millicores: number): string {
  return exactRatio(millicores, 1000);
}

/** Memory in GiB, from MiB, as a plain number for an input: 4096 → "4", 1536 → "1.5". */
export function memoryGiB(mib: number): string {
  return exactRatio(mib, 1024);
}

/**
 * Memory as a quantity for a label: whole GiB as "4Gi", anything else as exact MiB
 * ("1536Mi"), so a label never shows a rounded number for an odd edit.
 */
export function memoryQuantity(mib: number): string {
  return mib % 1024 === 0 ? `${mib / 1024}Gi` : `${mib}Mi`;
}

/**
 * One size's ceilings as a single readable clause for the picker:
 * `up to 4 CPU / 12Gi RAM / 25Gi disk`.
 *
 * ONLY THE LIMITS. A user choosing between sizes is choosing a CEILING ("will my build
 * fit?"); the request is a scheduling detail they cannot observe. "up to" qualifies all
 * three honestly: CPU and memory are cgroup ceilings, the disk is a volume of that size.
 */
export function sizeSummary(spec: WorkerSizeSpec): string {
  return `up to ${cpuCores(spec.cpu_limit_millicores)} CPU / ${memoryQuantity(spec.memory_limit_mib)} RAM / ${spec.data_gib}Gi disk`;
}

/**
 * The picker's option text: the uppercase size and its resource summary, so the
 * quantities are visible AT the moment of choosing.
 */
export function sizeOptionLabel(size: NamedWorkerSize): string {
  return `${sizeLabel(size.name)} — ${sizeSummary(size)}`;
}

/** A parsed human-unit input: the api integer, or the reason it cannot be one. */
export type ParsedQuantity = { ok: true; value: number } | { ok: false; error: string };

const DECIMAL = /^\d+(\.\d+)?$/;

/**
 * CPU cores typed by an admin → millicores, ×1000 rounded to an integer ("0.25" → 250).
 * Only obvious errors are caught here; the api is authoritative on every bound.
 */
export function parseCores(text: string): ParsedQuantity {
  const t = text.trim();
  if (!DECIMAL.test(t)) return { ok: false, error: "Enter a number of CPU cores, such as 0.5 or 2" };
  const value = Math.round(Number(t) * 1000);
  return value > 0 ? { ok: true, value } : { ok: false, error: "Must be more than 0 CPU" };
}

/**
 * Memory in GiB typed by an admin → MiB, ×1024, which must come out a whole MiB
 * ("1.5" → 1536; "0.1" is refused rather than rounded, since the pod would differ).
 */
export function parseMemoryGiB(text: string): ParsedQuantity {
  const t = text.trim();
  if (!DECIMAL.test(t)) return { ok: false, error: "Enter memory in GiB, such as 4 or 1.5" };
  const raw = Number(t) * 1024;
  const value = Math.round(raw);
  if (Math.abs(raw - value) > 1e-6) return { ok: false, error: "Must be a whole number of MiB (GiB × 1024)" };
  return value > 0 ? { ok: true, value } : { ok: false, error: "Must be more than 0 GiB" };
}

/** A disk size typed by an admin in whole GiB ("50" → 50). */
export function parseDiskGiB(text: string): ParsedQuantity {
  const t = text.trim();
  if (!/^\d+$/.test(t)) return { ok: false, error: "Enter a whole number of GiB" };
  const value = Number(t);
  return value > 0 ? { ok: true, value } : { ok: false, error: "Must be more than 0 GiB" };
}
