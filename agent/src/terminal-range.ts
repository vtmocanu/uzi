import type { CheckpointRange } from "./git.js";

const SHA = /^[0-9a-f]{40}$/;
const KEYS = new Set(["tipSha", "excludeSha", "scanFloorShas", "defaultBlobSha", "packExcludeShas"]);
const MAX_ITEMS = 256;

/** Canonical JSON: fixed key order; optional fields appear only when present (absent differs from `[]`). */
export function serializeCheckpointRange(range: CheckpointRange): string {
  const o: Record<string, unknown> = { tipSha: range.tipSha, excludeSha: range.excludeSha };
  if (range.scanFloorShas !== undefined) o.scanFloorShas = [...range.scanFloorShas];
  if (range.defaultBlobSha !== undefined) o.defaultBlobSha = range.defaultBlobSha;
  if (range.packExcludeShas !== undefined) o.packExcludeShas = [...range.packExcludeShas];
  return JSON.stringify(o);
}

function shaArray(v: unknown): string[] | undefined {
  if (!Array.isArray(v) || v.length > MAX_ITEMS) return undefined;
  return v.every(x => typeof x === "string" && SHA.test(x)) ? [...v] as string[] : undefined;
}

/** Strict inverse of {@link serializeCheckpointRange}; undefined on any deviation. */
export function parseCheckpointRange(json: string): CheckpointRange | undefined {
  let v: unknown;
  try { v = JSON.parse(json); } catch { return undefined; }
  if (!v || typeof v !== "object" || Array.isArray(v)) return undefined;
  const o = v as Record<string, unknown>;
  if (Object.keys(o).some(k => !KEYS.has(k))) return undefined;
  if (typeof o.tipSha !== "string" || !SHA.test(o.tipSha)) return undefined;
  if (typeof o.excludeSha !== "string" || !SHA.test(o.excludeSha)) return undefined;
  const out: CheckpointRange = { tipSha: o.tipSha, excludeSha: o.excludeSha };
  if ("scanFloorShas" in o) { const a = shaArray(o.scanFloorShas); if (!a) return undefined; out.scanFloorShas = a; }
  if ("defaultBlobSha" in o) {
    if (typeof o.defaultBlobSha !== "string" || !SHA.test(o.defaultBlobSha)) return undefined;
    out.defaultBlobSha = o.defaultBlobSha;
  }
  if ("packExcludeShas" in o) { const a = shaArray(o.packExcludeShas); if (!a) return undefined; out.packExcludeShas = a; }
  return out;
}
