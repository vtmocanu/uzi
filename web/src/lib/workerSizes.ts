// Hosted worker sizes (PRD #58): the curated presets a hosted worker can be
// provisioned at. This is the web-side mirror of the server registry (api/internal/
// workersize), exactly as workerTemplates.ts mirrors workertmpl.Names — a tiny
// curated list needs no API round-trip, and GET /workers/hosted/config deliberately
// does not ship it. Unlike the template list, this one has a drift gate:
// workerSizes.test.ts checks it against the cross-module golden the api generates.
//
// NAMES ONLY, and that is a decision rather than an omission. What cpu/memory/disk
// each preset actually buys is the CONTROLLER's preset table (Decision 1: the api may
// not know a pod spec), and that table does not exist yet — M3 writes it. Nobody has
// chosen the quantities, so displaying one here would mean inventing it, and a number
// M5 invents that M3 later contradicts is a lie to the user with no loud failure.
// Showing the quantities is deferred to M6, once there is an authority to read them
// from. Do not add a number to this file, or to the UI, before then.

/** Curated size names, in display order (smallest first). Lowercase on the wire. */
export const WORKER_SIZES = ["s", "m", "l"] as const;

export type WorkerSize = (typeof WORKER_SIZES)[number];

/**
 * The size a fresh provision form starts on. The smallest preset: it is the cheapest
 * choice against a per-user quota, and picking it for the user costs them the least
 * if they never look at the field.
 */
export const DEFAULT_WORKER_SIZE: WorkerSize = "s";

/**
 * Display spelling of a size name. Upper-case is for READING only — the wire value
 * is lowercase and nothing upper-cased is ever a value (workersize.Valid("M") is
 * false, and a Go test pins that). Keep this the only place the two diverge.
 */
export function sizeLabel(size: string): string {
  return size.toUpperCase();
}
