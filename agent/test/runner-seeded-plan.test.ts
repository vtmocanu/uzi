import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { StubExecutor, type RunContext, type ExecutorResult } from "../src/executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  fakeGitlab,
  gitlabClaim,
  installHarness,
  runner,
  runnerWith,
} from "./runner-harness.js";

installHarness();

// ── PRD #209 D4: the runner's three-way plan discriminator ───────────────────
//
// runner.ts folds `claim.plan_source === "seeded"` into the planApproved it hands the
// executor: planApproved = claim.plan_approved && (sessionId || seeded). These pin the
// four rows of D4's table AT THE RUNNER — the layer that owns the fact — by capturing
// the RunContext the executor receives (planApproved / seeded / approvedPlan). The
// executor-level "what the run then does" rows live in sdk-executor.test.ts and
// executor.test.ts; here the question is only what the runner resolves.
describe("RunRunner — seeded plan discriminator (PRD #209 D4)", () => {
  const SID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";

  /** A factory with a per-run HOME under `homeRoot`, capturing the RunContext. The HOME
   *  is what makes the resume preflight run (issue #105) — without it a claim's session
   *  id passes through unchecked. */
  function capturingFactory(
    homeRoot: string,
    seen: RunContext[],
  ): ExecutorFactory {
    return (runId) => ({
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          seen.push(ctx);
          return { branch: ctx.branch };
        },
      },
      homeDir: path.join(homeRoot, runId),
    });
  }

  /** Plant a transcript under this run's HOME so the preflight keeps the resume. Mirrors
   *  runner-resume-preflight.test.ts: a per-run HOME holds only this run's project dir,
   *  so the exact dir name is immaterial. */
  function plantTranscript(runHome: string, sessionId: string): void {
    const dir = path.join(
      runHome,
      ".claude",
      "projects",
      "-data-runner-repo-issue-x",
    );
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(path.join(dir, `${sessionId}.jsonl`), "{}\n");
  }

  // Row 2 (NEW): approved + NO session + SEEDED ⇒ planApproved true, so the executor
  // implements the seeded plan with no gate. This is the case the old `&& sessionId`
  // guard wrongly blocked.
  it("row 2: a fresh seeded run is approved with no session (skips the gate)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-seeded-"));
    const seen: RunContext[] = [];
    const claim = gitlabClaim(80, {
      plan_approved: true,
      plan_source: "seeded",
      plan_md: "# Seeded plan\n- do it",
      session_id: null,
    });
    try {
      await runnerWith(capturingFactory(homeRoot, seen), gitlab).execute(claim);
      assert.strictEqual(
        seen[0]?.planApproved,
        true,
        "a seeded run is approved even with no session",
      );
      assert.strictEqual(
        seen[0]?.seeded,
        true,
        "the seeded flag reaches the executor",
      );
      assert.strictEqual(seen[0]?.approvedPlan, "# Seeded plan\n- do it");
      assert.strictEqual(seen[0]?.sessionId, undefined);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // Row 3 (REGRESSION — Success Criterion 3): approved + session DROPPED + NOT seeded ⇒
  // planApproved false, so the executor RE-PLANS. A transcript lost mid-flight is not a
  // seeded plan and must never be treated as one, even though both arrive here as
  // "approved, no session".
  it("row 3: a dropped-session non-seeded run is NOT approved (must re-plan)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-seeded-"));
    const seen: RunContext[] = [];
    // session_id present but its transcript is NOT planted ⇒ the preflight drops it.
    const claim = gitlabClaim(81, {
      plan_approved: true,
      plan_source: "agent",
      plan_md: "# a worker plan",
      session_id: SID,
    });
    try {
      await runnerWith(capturingFactory(homeRoot, seen), gitlab).execute(claim);
      assert.strictEqual(
        seen[0]?.sessionId,
        undefined,
        "the dead session was dropped by the preflight",
      );
      assert.strictEqual(
        seen[0]?.planApproved,
        false,
        "a dropped-session non-seeded run must re-plan, never skip",
      );
      assert.strictEqual(seen[0]?.seeded, false);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // Row 1 (unchanged): approved + RESOLVABLE session + not seeded ⇒ planApproved true
  // (today's PRD #35 resume). Kept so the discriminator's four rows read together.
  it("row 1: an approved run with a resolvable session stays approved (PRD #35 resume)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-seeded-"));
    const seen: RunContext[] = [];
    const claim = gitlabClaim(82, {
      plan_approved: true,
      plan_source: "agent",
      plan_md: "# a worker plan",
      session_id: SID,
    });
    try {
      plantTranscript(path.join(homeRoot, claim.run_id), SID);
      await runnerWith(capturingFactory(homeRoot, seen), gitlab).execute(claim);
      assert.strictEqual(seen[0]?.sessionId, SID);
      assert.strictEqual(seen[0]?.planApproved, true);
      assert.strictEqual(seen[0]?.seeded, false);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // Row 4 (unchanged): not approved ⇒ planApproved false regardless of source.
  it("row 4: a not-approved run is never skipped", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-seeded-"));
    const seen: RunContext[] = [];
    const claim = gitlabClaim(83, {
      plan_approved: false,
      plan_source: "agent",
      session_id: null,
    });
    try {
      await runnerWith(capturingFactory(homeRoot, seen), gitlab).execute(claim);
      assert.strictEqual(seen[0]?.planApproved, false);
      assert.strictEqual(seen[0]?.seeded, false);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // Fail-safe (auditor-m2's surviving mutation): an OLDER server omits plan_source. The
  // discriminator MUST compare the literal "seeded", never `!== "agent"` — the latter
  // treats a missing field as seeded, reopening the D8 hole (skip the gate and implement an
  // unreviewed plan). With no plan_source and no session, planApproved must follow the
  // SESSION (false), not the source. This test fails under the `!== "agent"` refactor,
  // which the whole D4 row suite above does NOT catch.
  it("fail-safe: an absent plan_source is NOT seeded (planApproved follows the session)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-seeded-"));
    const seen: RunContext[] = [];
    // plan_source deliberately omitted (older server), approved, no session.
    const claim = gitlabClaim(84, {
      plan_approved: true,
      plan_md: "# a plan",
      session_id: null,
    });
    try {
      await runnerWith(capturingFactory(homeRoot, seen), gitlab).execute(claim);
      assert.strictEqual(
        seen[0]?.seeded,
        false,
        "an absent plan_source must not be treated as seeded",
      );
      assert.strictEqual(
        seen[0]?.planApproved,
        false,
        "no session + not seeded ⇒ must re-plan, never skip (D8 fail-safe)",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // D3 ⟨R⟩ invariant (PRD asked M2 to "assert, not assume"): a seeded run has a create-time
  // agent_selection persisted server-side, and SetRunRunning COALESCEs agent_source against
  // any non-null report — so if the worker echoed a selection on a state report it would
  // CLOBBER the create-time one. A seeded run takes the pre-approved path and NEVER enters
  // the autopilot gate branch (runner.ts's only site that reports agent_selection), so it
  // must send no such report. Driven through the REAL runner + stub so every /state body is
  // the one the server would receive.
  it("D3: a seeded run reports NO agent_selection (cannot clobber its create-time selection)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(85, {
      plan_approved: true,
      plan_source: "seeded",
      plan_md: "# Seeded plan\n- do it",
      session_id: null,
      // The create-time selection the server persisted and replays on the claim.
      agent_selection: { source: "own", exclusions: ["reviewer"] },
    });
    await runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab).execute(claim);
    const states = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body);
    assert.ok(
      states.every((s) => s.agent_selection === undefined),
      "a seeded run must never echo an agent_selection on a state report (D3 clobber guard)",
    );
    assert.ok(
      !states.some((s) => s.status === "awaiting_approval"),
      "a seeded run skips the gate entirely (pre-approved path taken)",
    );
    assert.ok(
      states.some((s) => s.status === "completed"),
      "the seeded run completed — the state stream is the full run, not a truncated one",
    );
  });
});
