import assert from "node:assert/strict";
import { parse } from "smol-toml";

/** Inspect semantic keys, including dotted/quoted outgoing override spellings. */
export function assertNoNativeServers(value: unknown): void {
  assert.ok(value !== null && typeof value === "object" && !Array.isArray(value),
    "outgoing thread config must be a non-null object, not an array");
  visit(value);
}

function visit(value: unknown): void {
  if (!value || typeof value !== "object") return;
  for (const [key, child] of Object.entries(value)) {
    let decoded: unknown;
    try { decoded = parse(`${key} = 0`); }
    catch { decoded = { [key]: 0 }; }
    const inspect = (node: unknown): void => {
      if (!node || typeof node !== "object") return;
      for (const [part, nested] of Object.entries(node)) {
        assert.notEqual(part, "mcp_servers", "no native MCP servers are configured");
        inspect(nested);
      }
    };
    inspect(decoded);
    visit(child);
  }
}
