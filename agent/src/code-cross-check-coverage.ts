import type { GitCache } from "./git.js";

export const CODE_COVERAGE_LIMITS = {
  entries: 128, pathBytes: 512, reasonBytes: 256, modelBytes: 32 * 1024,
  accountBytes: 128 * 1024, verdictBytes: 256 * 1024, inventoryBytes: 1024 * 1024,
  records: 16384,
} as const;
const REASONS = ["inventory_truncated", "inventory_failed", "invalid_judgments", "unknown_judgments",
  "judgments_truncated", "activity_incomplete", "coverage_invalid", "coverage_too_large"] as const;
type CoverageReason = typeof REASONS[number];
export interface CodeInventory {
  paths: string[];
  inventory_incomplete: boolean;
  omitted_paths: number;
  omitted_count_exact: boolean;
  reasons: CoverageReason[];
}
export interface CodeCoverage {
  version: 1;
  entries: Array<{ path: string; status: "examined" | "excluded" | "unassessed"; reason: string; recorded_read: boolean }>;
  inventory_incomplete: boolean;
  omitted_paths: number;
  omitted_count_exact: boolean;
  unknown_judgments: number;
  invalid_judgments: number;
  omitted_judgments: number;
  activity_complete: boolean;
  reasons: CoverageReason[];
}
const bytes = (s: string): number => Buffer.byteLength(s, "utf8");
const object = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);
const text = (v: unknown, cap: number): v is string => typeof v === "string" && bytes(v) <= cap
  && Buffer.from(v).toString("utf8") === v && !v.includes("\0");
const count = (v: unknown): v is number => Number.isSafeInteger(v) && (v as number) >= 0;
const failedInventory = (): CodeInventory => ({
  paths: [], inventory_incomplete: true, omitted_paths: 0, omitted_count_exact: false, reasons: ["inventory_failed"],
});

/** Worker-only denominator, independent of patch text and checker-authored judgments. */
export async function collectCodeInventory(
  git: Pick<GitCache, "readBare">, bare: string, base: string, head: string,
  options: { timeoutMs: number; signal?: AbortSignal },
): Promise<CodeInventory> {
  try {
    if (!/^[a-f0-9]{40}$/.test(base) || !/^[a-f0-9]{40}$/.test(head)) return failedInventory();
    const result = await git.readBare(bare,
      ["diff", "--no-ext-diff", "--no-textconv", "--name-status", "-z", "--find-renames", `${base}...${head}`],
      { ...options, maxBytes: CODE_COVERAGE_LIMITS.inventoryBytes, includeBytes: true });
    if (!result.bytes || result.bytes.length > CODE_COVERAGE_LIMITS.inventoryBytes) return failedInventory();
    const raw = Buffer.from(result.bytes);
    const inventory: CodeInventory = { paths: [], inventory_incomplete: result.truncated,
      omitted_paths: 0, omitted_count_exact: !result.truncated, reasons: [] };
    const seen = new Set<string>();
    let cursor = 0, records = 0;
    const next = (): Buffer | undefined => {
      if (records >= CODE_COVERAGE_LIMITS.records) return undefined;
      const end = raw.indexOf(0, cursor);
      if (end === -1) return undefined;
      const token = raw.subarray(cursor, end);
      cursor = end + 1;
      records++;
      return token;
    };
    while (cursor < raw.length) {
      const status = next()?.toString("ascii");
      if (!status || !/^(?:[AMDTUXB]|[RC][0-9]{1,3})$/.test(status)) {
        inventory.omitted_count_exact = false;
        break;
      }
      const paths = status.startsWith("R") || status.startsWith("C") ? 2 : 1;
      let complete = true;
      for (let i = 0; i < paths; i++) {
        const value = next();
        if (!value || value.length === 0) { complete = false; break; }
        const identity = value.toString("hex");
        if (seen.has(identity)) continue;
        seen.add(identity);
        const path = value.toString("utf8");
        if (value.length > CODE_COVERAGE_LIMITS.pathBytes || !Buffer.from(path).equals(value)
          || inventory.paths.length >= CODE_COVERAGE_LIMITS.entries) {
          inventory.omitted_paths++;
        } else inventory.paths.push(path);
      }
      if (!complete) { inventory.omitted_count_exact = false; break; }
    }
    if (!inventory.omitted_count_exact || inventory.omitted_paths > 0) {
      inventory.inventory_incomplete = true;
      inventory.reasons.push("inventory_truncated");
    }
    return inventory;
  } catch {
    // Cancellation/deadline still governs the surrounding check; no fabricated empty full inventory.
    return failedInventory();
  }
}

/** Missing/invalid coverage never invalidates the independently decoded findings. */
export function normalizeCodeCoverage(
  inventory: CodeInventory, judgments: unknown, recordedReads: ReadonlySet<string>, activityComplete: boolean,
): CodeCoverage {
  const account: CodeCoverage = { version: 1,
    entries: inventory.paths.map(path => ({ path, status: "unassessed", reason: "", recorded_read: false })),
    inventory_incomplete: inventory.inventory_incomplete, omitted_paths: inventory.omitted_paths,
    omitted_count_exact: inventory.omitted_count_exact, unknown_judgments: 0, invalid_judgments: 0,
    omitted_judgments: 0, activity_complete: activityComplete, reasons: [...inventory.reasons] };
  const reason = (code: CoverageReason): void => {
    account.inventory_incomplete = true;
    if (!account.reasons.includes(code)) account.reasons.push(code);
  };
  if (!activityComplete) reason("activity_incomplete");
  const entries = new Map(account.entries.map(entry => [entry.path, entry]));
  const seen = new Set<string>(), duplicates = new Set<string>();
  let encoded: string | undefined;
  try { encoded = JSON.stringify(judgments); } catch { /* not a JSON value */ }
  if (!Array.isArray(judgments)) {
    reason("coverage_invalid");
  } else if (encoded === undefined || bytes(encoded) > CODE_COVERAGE_LIMITS.modelBytes) {
    account.omitted_judgments = judgments.length;
    reason("judgments_truncated");
  } else {
    account.omitted_judgments = Math.max(0, judgments.length - CODE_COVERAGE_LIMITS.entries);
    if (account.omitted_judgments) reason("judgments_truncated");
    for (const raw of judgments.slice(0, CODE_COVERAGE_LIMITS.entries)) {
      if (!object(raw) || typeof raw.path !== "string") {
        account.invalid_judgments++; reason("invalid_judgments"); continue;
      }
      const entry = entries.get(raw.path);
      if (!entry) { account.unknown_judgments++; reason("unknown_judgments"); continue; }
      if (seen.has(raw.path)) {
        duplicates.add(raw.path);
        account.invalid_judgments++; reason("invalid_judgments"); continue;
      }
      seen.add(raw.path);
      if (!["examined", "excluded", "unassessed"].includes(raw.status as string)
        || !text(raw.reason, CODE_COVERAGE_LIMITS.reasonBytes)
        || (raw.status !== "unassessed" && raw.reason.length === 0)) {
        account.invalid_judgments++; reason("invalid_judgments"); continue;
      }
      entry.status = raw.status as typeof entry.status;
      entry.reason = raw.reason;
      entry.recorded_read = entry.status === "examined" && recordedReads.has(entry.path);
    }
  }
  if (Array.isArray(judgments) && encoded !== undefined && bytes(encoded) <= CODE_COVERAGE_LIMITS.modelBytes) {
    for (const raw of judgments.slice(CODE_COVERAGE_LIMITS.entries)) {
      if (object(raw) && typeof raw.path === "string" && seen.has(raw.path)) {
        duplicates.add(raw.path); account.invalid_judgments++; reason("invalid_judgments");
      }
    }
  }
  for (const path of duplicates) {
    const entry = entries.get(path)!;
    entry.status = "unassessed"; entry.reason = ""; entry.recorded_read = false;
  }
  while (bytes(JSON.stringify(account)) > CODE_COVERAGE_LIMITS.accountBytes && account.entries.length) {
    account.entries.pop();
    account.omitted_paths++;
    reason("inventory_truncated");
  }
  return account;
}

/** Stored wire account: legacy absence is null; malformed server evidence is refused. */
export function decodeCodeCoverage(value: unknown): CodeCoverage | null {
  if (value === undefined || value === null) return null;
  const invalid = (): never => { throw new Error("invalid code coverage account"); };
  if (!object(value) || bytes(JSON.stringify(value)) > CODE_COVERAGE_LIMITS.accountBytes
    || Object.keys(value).sort().join(",") !== "activity_complete,entries,invalid_judgments,inventory_incomplete,omitted_count_exact,omitted_judgments,omitted_paths,reasons,unknown_judgments,version"
    || value.version !== 1 || !Array.isArray(value.entries) || value.entries.length > CODE_COVERAGE_LIMITS.entries
    || typeof value.inventory_incomplete !== "boolean" || typeof value.omitted_count_exact !== "boolean"
    || typeof value.activity_complete !== "boolean"
    || ![value.omitted_paths, value.unknown_judgments, value.invalid_judgments, value.omitted_judgments].every(count)
    || !Array.isArray(value.reasons) || value.reasons.length > 8
    || value.reasons.some(r => !REASONS.includes(r as CoverageReason))) return invalid();
  const seen = new Set<string>();
  for (const entry of value.entries) {
    if (!object(entry) || Object.keys(entry).sort().join(",") !== "path,reason,recorded_read,status"
      || !text(entry.path, CODE_COVERAGE_LIMITS.pathBytes) || !entry.path || seen.has(entry.path)
      || !text(entry.reason, CODE_COVERAGE_LIMITS.reasonBytes)
      || !["examined", "excluded", "unassessed"].includes(entry.status as string)
      || (entry.status !== "unassessed" && entry.reason.length === 0)
      || (entry.recorded_read === true && entry.status !== "examined")
      || typeof entry.recorded_read !== "boolean") return invalid();
    seen.add(entry.path);
  }
  if ((!value.omitted_count_exact || (value.omitted_paths as number) > 0) && !value.inventory_incomplete) return invalid();
  return value as unknown as CodeCoverage;
}
