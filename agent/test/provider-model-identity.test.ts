import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { ProviderModelUnmappedError, resolveCodexProviderModelSlot, resolveProviderModel } from "../src/provider-binding.js";

// The TS half of the provider model identity contract (PRD #4574 M2). The Go half is
// api/internal/providers/identity_test.go, which resolves the same cases through ResolveModelID.
// Each side folds its own production resolution against the shared fixture,
// fixtures/provider-model-identity/cases.json. An unreadable fixture is fatal, never a skip.

interface IdentityCase {
  name: string;
  engine: "claude" | "codex";
  map: Record<string, { model: string }>;
  requested: string;
  explicit: boolean;
  resolved: string | null;
}

function readCases(): IdentityCase[] {
  const path = fileURLToPath(new URL("../../fixtures/provider-model-identity/cases.json", import.meta.url));
  return JSON.parse(readFileSync(path, "utf8")) as IdentityCase[];
}

// The target model id the agent sends upstream for a case, or throws ProviderModelUnmappedError.
function resolveTarget(c: IdentityCase): string {
  if (c.engine === "claude") {
    const picked = resolveProviderModel(c.requested, c.map);
    // An alias maps to its target; a value that is already a mapped target is returned as given.
    return c.map[picked]?.model ?? picked;
  }
  const slot = resolveCodexProviderModelSlot(c.requested, c.map as never, c.explicit);
  const target = c.map[slot]?.model;
  if (target === undefined || target === "") throw new ProviderModelUnmappedError(c.requested);
  return target;
}

describe("provider model identity contract (PRD #4574 M2)", () => {
  const cases = readCases();

  it("resolves each case exactly as the fixture states", () => {
    const seen = new Set<string>();
    for (const c of cases) {
      if (c.resolved === null) {
        assert.throws(() => resolveTarget(c), ProviderModelUnmappedError, c.name);
      } else {
        assert.equal(resolveTarget(c), c.resolved, c.name);
      }
      seen.add(c.name);
    }
    assert.equal(seen.size, cases.length, "case names must be unique");
    for (const c of cases) assert.ok(seen.has(c.name), `case not exercised: ${c.name}`);
    assert.ok(cases.length > 0);
    assert.ok(cases.some((c) => c.engine === "claude"), "no claude case");
    assert.ok(cases.some((c) => c.engine === "codex"), "no codex case");
    assert.ok(cases.some((c) => c.resolved === null), "no unresolved case");
  });
});
