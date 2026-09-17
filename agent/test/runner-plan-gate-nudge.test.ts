import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { makeClaim } from "./helpers.js";
import { type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { PlanVerdict } from "../src/steering.js";
import { api, fx, fakeGitlab, installHarness, runner, input } from "./runner-harness.js";

installHarness();

// PRD #1416 M5 — the WARN-ONLY plan-gate nudge. When a run's branch has a published floor P and the
// submitted plan proposes rewriting history, the gate emits ONE visible `status` run message before
// the plan flows through the verdict (BOTH modes), and on an AUTO-APPROVED run ALSO arms the M2
// worker-authoritative safety steer for the first implement turn. The verdict flow is untouched
// (never rejects, never blocks — D5: false positives are accepted). These drive a real runner clone
// through a fake executor that calls ctx.gatePlan(plan) and records the returned verdict + whatever
// ctx.pullSafetySteer hands back. Real bare + clone from the runner-harness fixture.

const ISO_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: ISO_ENV, encoding: "utf8" }).trim();
}

/** Publish a branch on the fixture origin with a commit of its own, so its tip P is a real forge
 *  tip distinct from `main`. Restores origin's HEAD to `main`. (Mirrors the M1/M2 test helpers.) */
function publishBranch(name: string): string {
  gitIn(fx.originPath, ["checkout", "-b", name]);
  fs.writeFileSync(path.join(fx.originPath, "PUBLISHED.md"), `# published work on ${name}\n`);
  gitIn(fx.originPath, ["add", "PUBLISHED.md"]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", `published commit on ${name}`]);
  const sha = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
  gitIn(fx.originPath, ["checkout", "main"]);
  return sha;
}

interface Obs {
  verdict?: PlanVerdict;
  steer?: string;
}

/** An executor that runs the plan gate with a caller-supplied plan and records the verdict it
 *  returned plus whatever the safety-steer slot then held (drained exactly once, as an executor
 *  loop top would). Returns without a real finalize expectation. */
function planGateExecutor(planMd: string): { executor: Executor; obs: Obs } {
  const obs: Obs = {};
  const executor: Executor = {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      obs.verdict = await ctx.gatePlan!(planMd);
      // The steer is drained AFTER the gate resolves — exactly the order both executors use at
      // their loop top, before the first buildImplementPrompt.
      obs.steer = ctx.pullSafetySteer?.();
      return { branch: ctx.branch };
    },
  };
  return { executor, obs };
}

function taskClaim(branch: string, overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  return makeClaim({
    run_id: (overrides.run_id as string | undefined) ?? randomUUID(),
    kind: "task",
    issue_iid: null,
    issue_title: "Handoff: published-branch work",
    issue_description: "Continue the work on the already-published branch.",
    branch,
    open_mr: false,
    repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
    last_seq: 0,
    secrets: {
      forge_pat: "fixture-forge-pat-000000",
      anthropic_oauth_token: "dummy-oauth-do-not-scan",
    },
    ...overrides,
  });
}

/** The plan-gate nudge status lines the worker emitted for a run (M5's own wording, distinct from
 *  M2's "history was rewritten below its published tip" mid-run line). */
const nudgeStatuses = (runId: string): string[] =>
  api
    .messages(runId)
    .filter(
      (m) =>
        m.kind === "status" &&
        String(m.payload.text).includes("the plan proposes rewriting history on a branch published at"),
    )
    .map((m) => String(m.payload.text));

// A plan that literally proposes a rebase of published commits — the D5-accepted trigger case.
const REWRITE_PLAN = "# PLAN\n- git rebase the 23 published commits onto main\n- fix the eight findings";
// A plan with no rewrite verbs — the negative control.
const CLEAN_PLAN = "# PLAN\n- add a new commit for the fix\n- integrate main with git merge, never rebase";

describe("RunRunner — plan-gate rewrite nudge (PRD #1416 M5)", () => {
  it("AUTO-APPROVE: a rewrite plan on a published branch emits ONE nudge AND arms the steer", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/nudge-auto";
    const P = publishBranch(branch);
    const { executor, obs } = planGateExecutor(REWRITE_PLAN);
    const claim = taskClaim(branch, { auto_approve: true });
    await runner(executor, gitlab).execute(claim).catch(() => undefined);

    const statuses = nudgeStatuses(claim.run_id);
    assert.equal(statuses.length, 1, "exactly one plan-gate nudge status line");
    assert.match(statuses[0]!, new RegExp(P.slice(0, 12)), "the nudge names the published tip's short sha");
    assert.match(statuses[0]!, /fast-forward only/);
    // The verdict flow is untouched: an auto-approved run still returns its normal approve verdict.
    assert.equal(obs.verdict?.kind, "approve", "auto-approve still returns approve (verdict unchanged)");
    // The M2 steer was armed with the PLAN-TIME preventive body (composePlanGateNudge), NOT the
    // detected-rewrite M2 recipe (composeSafetySteer, which says `git merge -s ours`).
    assert.notEqual(obs.steer, undefined, "the safety steer was armed under auto-approve");
    assert.match(obs.steer!, new RegExp(P), "the steer names the published tip P");
    assert.match(obs.steer!, /already published/i);
    assert.match(obs.steer!, /Add new commits on top/, "it carries the plan-gate preventive body");
    assert.doesNotMatch(obs.steer!, /-s ours/, "it is the PLAN-TIME body, not the M2 detected-rewrite recipe");
    // The run never parked at the plan gate (auto-approve).
    assert.ok(
      !api.states.some((s) => s.runId === claim.run_id && s.body.status === "awaiting_approval"),
      "an auto-approved run never enters awaiting_approval",
    );
  });

  it("fresh branch (P undefined): a rewrite plan emits NOTHING — no status, no steer", async () => {
    const { gitlab } = fakeGitlab();
    const { executor, obs } = planGateExecutor(REWRITE_PLAN);
    // A task branch that never existed on the forge → P is null → nudge is gated on P being set.
    const claim = taskClaim(`uzi/task/${randomUUID()}`, { auto_approve: true });
    await runner(executor, gitlab).execute(claim).catch(() => undefined);

    assert.equal(nudgeStatuses(claim.run_id).length, 0, "no published floor → no nudge");
    assert.equal(obs.steer, undefined, "no published floor → no steer");
    assert.equal(obs.verdict?.kind, "approve", "the verdict is still a normal approve");
  });

  it("published branch, clean plan (no rewrite verbs): emits NOTHING", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/nudge-clean";
    publishBranch(branch);
    const { executor, obs } = planGateExecutor(CLEAN_PLAN);
    const claim = taskClaim(branch, { auto_approve: true });
    await runner(executor, gitlab).execute(claim).catch(() => undefined);

    assert.equal(nudgeStatuses(claim.run_id).length, 0, "a plan with no rewrite verbs does not fire");
    assert.equal(obs.steer, undefined, "and arms no steer");
    assert.equal(obs.verdict?.kind, "approve");
  });

  // D5: the nudge is a warn-only regex over prose, so a plan that says "do NOT git rebase" still
  // fires. This is an ACCEPTED false positive — one spurious status line costs nothing, and a false
  // negative would be caught by the M2 steer / M3 bridge.
  it("D5 false positive: a plan that says \"do not git rebase\" still fires the nudge (accepted)", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/nudge-falsepos";
    publishBranch(branch);
    const { executor, obs } = planGateExecutor(
      "# PLAN\n- do NOT `git rebase` the published commits; integrate main with `git merge` instead",
    );
    const claim = taskClaim(branch, { auto_approve: true });
    await runner(executor, gitlab).execute(claim).catch(() => undefined);

    assert.equal(nudgeStatuses(claim.run_id).length, 1, "an accepted false positive still fires exactly once");
    assert.notEqual(obs.steer, undefined, "and arms the steer under auto-approve (accepted)");
  });

  it("HUMAN-GATED: a rewrite plan emits the nudge, parks + awaits the verdict, but arms NO steer", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/nudge-human";
    publishBranch(branch);
    const { executor, obs } = planGateExecutor(REWRITE_PLAN);
    // No auto_approve → the gate parks at awaiting_approval and awaits a verdict from /inputs.
    const claim = taskClaim(branch);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(executor, gitlab).execute(claim).catch(() => undefined);

    // The nudge fires in BOTH modes.
    assert.equal(nudgeStatuses(claim.run_id).length, 1, "the nudge fires on the human-gated branch too");
    // The verdict flow is unaffected: the run parked at the gate and resumed on the approve input.
    assert.ok(
      api.states.some((s) => s.runId === claim.run_id && s.body.status === "awaiting_approval"),
      "a human-gated matching plan still posts awaiting_approval and awaits the verdict",
    );
    assert.equal(obs.verdict?.kind, "approve", "and returns the approve verdict as before");
    // No steer on the human-gated branch: a human saw the plan + the nudge and can revise/reject.
    assert.equal(obs.steer, undefined, "no steer is armed on the human-gated branch");
  });
});
