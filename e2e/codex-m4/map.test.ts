// PRD #1287 C1 — the clause map is GENERATED from the registry, not a separately-drifting
// committed markdown (D3). This asserts renderClauseMap(ALL_CLAUSES) covers EVERY row (each id,
// adapter, and layer appears), so map + registry can never silently diverge at C1.

import { describe, it } from "node:test";
import assert from "node:assert/strict";

import type { ClauseRow } from "./clause.js";
import { renderClauseMap } from "./map.js";
import { ALL_CLAUSES } from "./registry.js";

describe("renderClauseMap", () => {
  const rendered = renderClauseMap(ALL_CLAUSES);

  it("is a markdown table generated from the registry", () => {
    assert.match(rendered, /clause map/);
    assert.match(rendered, /\| id \| adapter \| layer \|/);
  });

  it("covers every registry row (id present exactly once)", () => {
    for (const clause of ALL_CLAUSES) {
      const occurrences = rendered.split(`| ${clause.id} |`).length - 1;
      assert.equal(occurrences, 1, `clause "${clause.id}" must appear exactly once in the map`);
    }
  });

  it("records each row's layer and adapter", () => {
    for (const clause of ALL_CLAUSES) {
      const row = rendered.split("\n").find((line) => line.startsWith(`| ${clause.id} |`));
      assert.ok(row !== undefined, `row for ${clause.id}`);
      assert.ok(row.includes(`| ${clause.adapter} |`), `${clause.id} adapter`);
      assert.ok(row.includes(`| ${clause.layer} |`), `${clause.id} layer`);
    }
  });

  it("escapes a literal backslash before a table delimiter", () => {
    const clause: ClauseRow = {
      id: "escape-probe",
      adapter: "codex",
      layer: "U",
      family: String.raw`x\|y`,
      seam: "probe",
      positiveControl: "probe",
      negativeOracle: "probe",
      intendedOutcome: "probe",
      tests: [],
    };
    const row = renderClauseMap([clause]).split("\n")
      .find((line) => line.startsWith(`| ${clause.id} |`));
    assert.ok(row !== undefined, "escape-probe row");
    assert.ok(row.includes(`| ${String.raw`x\\\|y`} |`), row);
  });

  it("reports a total that matches the registry size", () => {
    assert.match(rendered, new RegExp(`Total clauses: ${ALL_CLAUSES.length} `));
  });
});
