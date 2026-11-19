import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import { runEnvForClaim } from "../src/run-env-merge.js";
import { catalogue } from "./run-env-catalogue.js";

// PRD #4416 M2: only the lead run lanes (the Claude SDK executor and the Codex run executor)
// receive repository variables. Every other lane that builds an environment or spawns a model
// never does, even when the repo has variables.

const SRC = new URL("../src/", import.meta.url);
const read = (rel: string): string => readFileSync(new URL(rel, SRC), "utf8");

/** Every non-lead lane's source: environment builders and model spawners. None may name the
 *  variables, so none can pass them to buildSdkEnv / buildCommandEnv. */
const NON_RECEIVING_LANES = [
  "chat-executor.ts", "chat-runner.ts", "claude-cross-check.ts", "isolated-executor.ts", "isolated-runner.ts", "claude-advice-harness.ts",
  "job-runner.ts", "judge-runner.ts", "review-runner.ts", "cross-check-runner.ts", "codex/cross-check.ts", "codex/codex-advice-harness.ts",
  "codex/launcher.ts", "codex/launch-cli.ts", "summary-runner.ts",
];

describe("lanes that never receive repository variables", () => {
  for (const lane of NON_RECEIVING_LANES) {
    it(`${lane} neither names the variables nor merges them`, () => {
      const source = read(lane);
      assert.ok(!/\brunEnv\b|\brun_env\b|mergeRunEnv|RunEnvInput|RunEnvVar/.test(source), `${lane} references repository variables`);
    });
  }

  it("the claim helper hands variables only to the lead kinds", () => {
    const secrets = { run_env: catalogue() };
    for (const kind of ["issue", "prompt", "self_improve", "ci_fix", "mr_rework", "task", undefined]) {
      assert.equal(runEnvForClaim({ kind, secrets }).length, secrets.run_env.length, `${String(kind)} receives`);
    }
    for (const kind of ["chat", "judge", "job", "cross_check"]) {
      assert.equal(runEnvForClaim({ kind, secrets }).length, 0, `${kind} never receives`);
    }
    assert.equal(runEnvForClaim({ kind: "task", review_target_run_id: "abc", secrets }).length, 0, "a task-review run never receives");
  });
});
