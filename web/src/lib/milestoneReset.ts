import type { Run } from "./apiTypes";

// Compare JSON values recursively: object key order is irrelevant; array order is not.
export function canonicalValue(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalValue).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.entries(value).sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0)
      .map(([key, v]) => `${JSON.stringify(key)}:${canonicalValue(v)}`).join(",")}}`;
  }
  return JSON.stringify(value) ?? "undefined";
}

export function changedMilestones(run: Run): boolean {
  return run.milestones != null &&
    canonicalValue(run.milestones) !== canonicalValue(run.milestones_candidate ?? null);
}

