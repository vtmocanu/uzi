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
    // Pinned here, independent of the fixture: deleting or renaming a fixture case turns this red.
    // Keep in step with the name list in api/internal/providers/identity_test.go.
    const expected = [
      "claude alias present", "claude alias absent", "claude target value maps to itself",
      "claude non-alias value matching no target", "claude blank goes through sonnet",
      "claude inherit goes through sonnet", "claude whitespace-only goes through sonnet",
      "codex key with a differing target", "codex key absent and not explicit gives default",
      "codex key absent and explicit is unresolved", "codex blank gives default",
      "codex explicit blank is unresolved", "codex requested default literal without explicit",
      "codex requested default literal with explicit",
    ];
    assert.deepEqual([...cases.map((c) => c.name)].sort(), [...expected].sort(), "fixture case names");
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
    assert.ok(cases.length > 0);
    assert.ok(cases.some((c) => c.engine === "claude"), "no claude case");
    assert.ok(cases.some((c) => c.engine === "codex"), "no codex case");
    assert.ok(cases.some((c) => c.resolved === null), "no unresolved case");
  });
});
