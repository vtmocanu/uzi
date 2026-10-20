import { createHash } from "node:crypto";

const shards = ["1/2", "2/2"] as const;

function canonicalPath(value: unknown): value is string {
  return typeof value === "string"
    // eslint-disable-next-line no-control-regex -- Reject control bytes in canonical filenames.
    && /^test\/[^/\\\x00-\x1f\x7f-\x9f\u061c\u200e\u200f\u2028-\u202e\u2066-\u2069]+\.test\.ts$/.test(value);
}

/** Pin validation also runs for an unsharded invocation; additions need no pins. */
export function selectAgentTests(
  inventory: readonly string[],
  pins: unknown,
  shard?: string,
): string[] {
  const discovered = new Set(inventory);
  if (inventory.length === 0 || discovered.size !== inventory.length || inventory.some((file) => !canonicalPath(file))) {
    throw new Error("Inventory must contain nonempty unique canonical test/*.test.ts paths");
  }
  if (pins === null || typeof pins !== "object" || Array.isArray(pins)
    || Object.keys(pins).sort().join(",") !== shards.join(",")) {
    throw new Error('Pins must have exactly the keys "1/2" and "2/2"');
  }
  const assignments = new Map<string, string>();
  for (const key of shards) {
    const entries: unknown = (pins as Record<string, unknown>)[key];
    if (!Array.isArray(entries)) throw new Error(`Pins for ${key} must be an array`);
    let previous: string | undefined;
    for (const file of entries) {
      if (!canonicalPath(file) || !discovered.has(file)) {
        throw new Error(`Invalid or stale pin: ${String(file)}`);
      }
      if (assignments.has(file) || (previous !== undefined && previous >= file)) {
        throw new Error(`Pins must be sorted and unique within and across shards: ${file}`);
      }
      assignments.set(file, key);
      previous = file;
    }
  }
  if (shard !== undefined && !shards.some((key) => key === shard)) {
    throw new Error('Shard must be "1/2" or "2/2"');
  }
  const selected = [...inventory].sort().filter((file) => {
    if (shard === undefined) return true;
    const assigned = assignments.get(file)
      ?? shards[createHash("sha256").update(file, "utf8").digest()[0]! % 2];
    return assigned === shard;
  });
  if (shard !== undefined && selected.length === 0) throw new Error(`Empty shard: ${shard}`);
  return selected;
}
