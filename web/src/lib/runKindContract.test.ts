import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { RUN_KINDS, isJudgeEligible, type RunKind } from "./runKind";

// The run-kind cross-language contract (PRD #983). This is the WEB HALF; the Go half
// pins api/internal/runkind to the SAME shared fixtures/run-kinds/registry.json. Neither
// reads the other: each folds its OWN production copy of the kind set against the SAME
// recorded registry, so a failure names the side that drifted. The fixture is the
// authoritative mirror of the DB runs_kind_check; production web cannot read it at
// runtime (it lives at the repo root, above web/), which is why runKind.ts hard-codes
// RUN_KINDS and isJudgeEligible and this test is the pin that keeps them honest.
function read(name: string): string {
  const url = new URL(`../../../fixtures/run-kinds/${name}`, import.meta.url);
  try {
    return readFileSync(url, "utf8");
  } catch (err) {
    throw new Error(
      `fixture unreadable: ${name}: ${String(err)} -- this contract asserts nothing ` +
        `without it, and skipping would look identical to passing`,
    );
  }
}

type Registry = { kinds: string[]; judge_eligible: string[] };

const registry = JSON.parse(read("registry.json")) as Registry;

describe("run-kind registry contract", () => {
  it("RUN_KINDS mirrors the fixture's kinds, in order", () => {
    // The annotation pins each element to the RunKind union, so a drift between the
    // literal tuple and the exported type is a compile error, not just a runtime one.
    const kinds: readonly RunKind[] = RUN_KINDS;
    expect([...kinds]).toEqual(registry.kinds);
  });

  it("isJudgeEligible over RUN_KINDS reproduces the fixture's judge_eligible, in order", () => {
    const eligible: RunKind[] = RUN_KINDS.filter((k) => isJudgeEligible(k));
    expect(eligible).toEqual(registry.judge_eligible);
  });
});
