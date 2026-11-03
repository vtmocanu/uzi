import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { driftedColumns, type TemplateContent } from "./agentTemplates";
import { sameContent } from "../mocks/mockApi";
import type { AgentTemplate, BuiltinDefinition } from "./api";

// The agent-template drift-predicate cross-language contract (issue #223 item 3).
// This is the VITEST HALF; the Go half is api/internal/agenttmpl/drift_contract_test.go.
// Neither reads the other: each folds the SAME (shipped, stored) case table with its OWN
// production drift predicate and compares against the SAME recorded expectation, so a
// failure names the side that drifted. fixtures/run-usage/ (issue #195) is the in-repo
// precedent for the shape.
//
// WHAT THIS PINS. "Has this row drifted from the definition this binary ships?" has three
// implementations and compare.go's own doc comment names their divergence as the hazard it
// exists to prevent: agenttmpl.SameContent (the Go half), mockApi/agents.ts's sameContent and
// agentTemplates.ts's driftedColumns (this half). No divergence can be CONSTRUCTED today --
// all three fold null/"" and null/[] identically, compare tools order-sensitively and never
// trim -- which is exactly why the agreement is pinned to a shared artifact before a fourth
// consumer makes a future divergence consequential.
//
// This half pins BOTH TS predicates: driftedColumns against expected.columns (exact, in
// display order), and sameContent against !expected.differs. The Go half reads differs only.
//
// 🔴 THE GO HALF NEEDS -count=1. The fixture is at the repo root, ABOVE api/, so every byte
// of it is outside that module and contributes NOTHING to the package's cache key: a
// fixture-only edit leaves `go test` printing "ok (cached)". The vitest half has no such
// cache and needs no flag. The two halves are NOT symmetric.

const FIXTURE = "../../../fixtures/agent-template-drift/cases.json";

type FixtureContent = Omit<TemplateContent, "codex_model"> & { codex_model?: string | null };

interface DriftCase {
  name: string;
  shipped: FixtureContent;
  stored: FixtureContent;
  expected: { differs: boolean; columns: string[] };
}
interface DriftTable {
  note: string;
  cases: DriftCase[];
}

function loadTable(): DriftTable {
  const url = new URL(FIXTURE, import.meta.url);
  let raw: string;
  try {
    raw = readFileSync(url, "utf8");
  } catch (err) {
    // A missing or unreadable fixture is a fatal, never a skip: this contract asserts
    // nothing without it, and skipping would look identical to passing.
    throw new Error(`drift fixture unreadable: ${String(err)}`);
  }
  return JSON.parse(raw) as DriftTable;
}

// sameContent takes the stored row and the shipped definition. Only the five content
// columns vary; the rest are inert defaults, present because AgentTemplate requires them.
function asRow(c: FixtureContent): AgentTemplate {
  return {
    id: "00000000-0000-0000-0000-000000000000",
    name: "coder",
    description: c.description,
    model: c.model,
    codex_model: c.codex_model ?? null,
    tools: c.tools,
    prompt_body: c.prompt_body,
    is_builtin: true,
    scope: "builtin",
    user_id: null,
    updated_by: null,
    differs_from_builtin: false,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
}
function asDef(c: FixtureContent): BuiltinDefinition {
  return {
    name: "coder",
    description: c.description,
    model: c.model,
    codex_model: c.codex_model ?? null,
    tools: c.tools,
    prompt_body: c.prompt_body,
  };
}

describe("agent-template drift-predicate contract (issue #223 item 3)", () => {
  const table = loadTable();

  it("the fixture holds cases", () => {
    expect(table.cases.length).toBeGreaterThan(0);
  });

  for (const c of table.cases) {
    it(`driftedColumns names the drifted columns: ${c.name}`, () => {
      expect(driftedColumns(asDef(c.shipped), asRow(c.stored))).toEqual(c.expected.columns);
    });
    it(`sameContent agrees on drift: ${c.name}`, () => {
      expect(sameContent(asRow(c.stored), asDef(c.shipped))).toBe(!c.expected.differs);
    });
    it(`fixture integrity: differs matches columns non-empty: ${c.name}`, () => {
      expect(c.expected.differs).toBe(c.expected.columns.length > 0);
    });
  }

  it("normalizes a legacy fixture with an absent Codex pin to null", () => {
    const legacy: FixtureContent = { description: "d", model: null, tools: null, prompt_body: "b" };
    const explicit = { ...legacy, codex_model: null };
    expect(asRow(legacy).codex_model).toBeNull();
    expect(asDef(legacy).codex_model).toBeNull();
    expect(driftedColumns(asDef(legacy), asRow(explicit))).toEqual([]);
    expect(sameContent(asRow(legacy), asDef(explicit))).toBe(true);
  });

  it("reports all five columns in display order", () => {
    const shipped: TemplateContent = {
      description: "d", model: "sonnet", codex_model: null, tools: null, prompt_body: "b",
    };
    const current: TemplateContent = {
      description: "edited", model: "opus", codex_model: "gpt-6-sol", tools: ["Read"], prompt_body: "edited",
    };
    expect(driftedColumns(shipped, current)).toEqual(["description", "model", "codex model", "tools", "prompt body"]);
  });

  // The load-bearing discriminating cases must stay: an implementation that sorted tools,
  // trimmed a field or treated null distinctly from empty would redden exactly these, so a
  // "tidied" fixture that dropped them would silently weaken the contract.
  it("keeps the load-bearing cases (do not tidy)", () => {
    const names = new Set(table.cases.map((c) => c.name));
    for (const required of [
      "identical",
      "description-trailing-space-not-trimmed",
      "model-null-vs-empty-agree",
      "model-whitespace-not-trimmed",
      "tools-reorder",
      "tools-null-vs-empty-agree",
      "prompt-body-trailing-newline-not-trimmed",
      "all-four-differ-in-display-order",
      "codex-model-value-change",
      "codex-model-null-vs-empty-agree",
      "model-moved-between-harnesses",
    ]) {
      expect(names.has(required)).toBe(true);
    }
  });
});
