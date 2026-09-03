import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import type { ClaimResponse } from "../src/protocol.js";
import { RUN_KIND_PROFILES } from "../src/run-kind.js";

// The TS half of the PRD #1062 M3 checkpoint branch-derivation cross-language contract;
// the Go half is api/internal/workersvc/checkpoint_branch_contract_test.go. Neither reads
// the other: each folds its OWN production derivation and compares against the SAME
// hand-authored fixture, fixtures/checkpoint-branch/cases.json. This side folds the
// worker's actual per-kind derivation (RUN_KIND_PROFILES.self_improve.cloneBranch for a
// self_improve run; the `agent/issue-${iid}` createOrAttachRunnerClone default for an
// issue run, which has no cloneBranch profile). See the fixture's README.

interface CheckpointBranchCases {
  eligible: Array<{ kind: string; run_id: string; issue_iid: number; branch: string }>;
  ineligible: string[];
}

// A throw-on-unreadable read (mirroring publish-contract's style): a missing/unreadable
// fixture is a fatal, never a skip — a skipped contract asserts nothing and would look
// identical to passing.
function readFixture(): CheckpointBranchCases {
  const path = fileURLToPath(
    new URL("../../fixtures/checkpoint-branch/cases.json", import.meta.url),
  );
  const raw = readFileSync(path, "utf8");
  return JSON.parse(raw) as CheckpointBranchCases;
}

describe("checkpoint branch derivation contract (PRD #1062 M3)", () => {
  const cases = readFixture();

  it("derives each eligible case's branch exactly as the fixture states", () => {
    const seen = new Set<string>();

    for (const c of cases.eligible) {
      seen.add(c.kind);
      if (c.kind === "self_improve") {
        // cloneBranch ignores the claim for self_improve; a minimal cast satisfies the
        // compiler without constructing every ClaimResponse field.
        const claim = { issue_iid: c.issue_iid } as unknown as ClaimResponse;
        const derived = RUN_KIND_PROFILES.self_improve.cloneBranch?.(claim, c.run_id);
        assert.strictEqual(
          derived?.branch,
          c.branch,
          `self_improve run ${c.run_id} should derive branch ${c.branch}`,
        );
      } else if (c.kind === "issue") {
        // The issue kind has no cloneBranch profile: createOrAttachRunnerClone derives
        // `agent/issue-${issue_iid}`.
        assert.strictEqual(
          `agent/issue-${c.issue_iid}`,
          c.branch,
          `issue run should derive branch ${c.branch}`,
        );
        assert.strictEqual(
          RUN_KIND_PROFILES.issue.cloneBranch,
          undefined,
          "issue kind must have no cloneBranch profile (it falls through to createOrAttachRunnerClone)",
        );
      } else {
        assert.fail(`fixture eligible case has an unhandled kind: ${c.kind}`);
      }
    }

    // Every eligible kind in the fixture was exercised.
    for (const c of cases.eligible) {
      assert.ok(seen.has(c.kind), `eligible kind ${c.kind} must be exercised`);
    }
    assert.strictEqual(
      seen.size,
      new Set(cases.eligible.map((c) => c.kind)).size,
      "the seen-set must cover exactly the fixture's eligible kinds",
    );
  });
});
