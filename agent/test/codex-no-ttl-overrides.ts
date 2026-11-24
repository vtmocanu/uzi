import assert from "node:assert/strict";
import { parse } from "smol-toml";

/** Parse emitted configuration so string values cannot masquerade as override fields. */
export function assertNoTtlOverridesToml(toml: string, construction: string): Record<string, unknown> {
  const config = parse(toml);
  assertNoTtlOverrides(config, construction);
  return config;
}

/** Inspect structured field paths, never prompt/string values. This checks worker
 * construction invariance, not provider cache duration or billing. */
export function assertNoTtlOverrides(value: unknown, construction: string): void {
  assert.ok(value !== null && typeof value === "object" && !Array.isArray(value),
    `${construction}: no TTL overrides requires a structured construction`);
  const visit = (node: unknown, path: string[]): void => {
    if (node === null || typeof node !== "object") return;
    for (const [key, child] of Object.entries(node)) {
      // Captures nested, dotted and quoted field spellings in parsed config/RPC objects.
      const parts = key.split(".").map(part => part.trim().replace(/^["']|["']$/g, ""));
      const next = [...path, ...parts];
      const forbidden = next.some((part, index) =>
        part === "prompt_cache_ttl" || part === "prompt_cache_retention" ||
        ((part === "prompt_cache_options" || part === "prompt_cache_breakpoint") && next[index + 1] === "ttl"));
      assert.equal(forbidden, false, `${construction}: no TTL overrides at ${next.join(".")}`);
      visit(child, next);
    }
  };
  visit(value, []);
}
