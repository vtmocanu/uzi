import assert from "node:assert/strict";
import { it, mock } from "node:test";
import { fx, git, installHarness } from "./runner-harness.js";
import { watchFreezes, type HeldServer } from "./runner-held-publication-fixture.js";

installHarness();

it("freeze observer distinguishes new coverage from post-send verification", async () => {
  let acknowledged = false;
  let fingerprint = "a".repeat(64);
  mock.method(git, "buildRecoveryCoverage", async () => ({
    sha: "b".repeat(40), fingerprint, coverageRef: `refs/uzi-coverage/run/7/${fingerprint}`,
    originalShas: ["c".repeat(40)], currentSha: "c".repeat(40),
  }));
  const seen = watchFreezes({ failedAcked: () => acknowledged } as HeldServer);
  const context = { runId: "run", generation: 7, kind: "issue" as const,
    branch: "task", barePath: git.barePathFor(fx.originPath),
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: "c".repeat(40) } };
  const build = () => git.buildRecoveryCoverage(context.barePath, context, ["c".repeat(40)], "c".repeat(40));
  await build();
  acknowledged = true;
  await build();
  assert.deepEqual(seen.failedAckedAtFreeze, [false], "re-verifying the same aggregate is not a second freeze");
  fingerprint = "d".repeat(64);
  await build();
  assert.deepEqual(seen.failedAckedAtFreeze, [false, true], "a genuinely new post-send coverage identity stays detectable");
});
