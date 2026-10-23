import assert from "node:assert/strict";
import { test } from "node:test";
import { createCriteria } from "./criteria.js";

test("failed criteria retain labels and fail the result after independent work completes", async () => {
  const failures: string[] = [];
  const criteria = createCriteria((label) => { failures.push(label); });
  assert.doesNotThrow(() => criteria.requireCriterion("Nix open/sentinel boundary", false));
  criteria.requireCriterion("command Devbox exits nonzero", true);
  assert.doesNotThrow(() => criteria.requireCriterion("command Devbox specifically denies Nix big-lock", false));

  let completed = false;
  await Promise.resolve().then(() => { completed = true; });

  assert.equal(completed, true);
  assert.deepEqual(failures, [
    "Nix open/sentinel boundary",
    "command Devbox specifically denies Nix big-lock",
  ]);
  assert.deepEqual(criteria.result(), {
    status: "FAIL",
    exitCode: 1,
    criterionFailures: [
      "Nix open/sentinel boundary",
      "command Devbox specifically denies Nix big-lock",
    ],
  });
});

test("all passing criteria produce PASS and zero exit status without failure callbacks", () => {
  const failures: string[] = [];
  const criteria = createCriteria((label) => { failures.push(label); });
  criteria.requireCriterion("Nix open/sentinel boundary", true);
  criteria.requireCriterion("command Devbox exits nonzero", true);
  criteria.requireCriterion("command Devbox specifically denies Nix big-lock", true);
  assert.deepEqual(failures, []);
  assert.deepEqual(criteria.result(), { status: "PASS", exitCode: 0, criterionFailures: [] });
});
