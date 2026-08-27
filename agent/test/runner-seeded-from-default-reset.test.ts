import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { GitCache, type RunnerClone } from "../src/git.js";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness } from "./runner-harness.js";

installHarness();

// PRD #628 M4 — the worker-side milestone-reset SIGNAL.
//
// On a cross-worker re-claim that reseeds from the DEFAULT branch (seededFrom ===
// "default", no committed work recovered), the runner must emit ONE dedicated run-start
// `running` report carrying `seeded_from_default: true`, which the server keys the
// ClearRunMilestonesCompleted reset on. The invariants under test:
//   - it fires exactly ONCE on a `seededFrom === "default"` reseed, on a `running` report;
//   - it is NEVER set on an iteration heartbeat (a per-heartbeat clear would wipe live
//     progress — the whole reason the signal is one-shot);
//   - it is NOT emitted when the reseed RECOVERED the tree (seededFrom "checkpoint" or
//     "tracking"), because those milestones are legitimately preserved. The key is the TREE
//     signal, never the session signal resume_lineage_break.

/** A GitCache that forces the reseed's `seededFrom` to a fixed value, so the runner's
 *  keying (the M4 change) is isolated from the git-level production of that value — which
 *  is covered end to end by the PRD #218 / #628 M3 git tests. Everything else (the real
 *  clone, base, checkout) is delegated to the production reseed. */
class ForcedSeedGit extends GitCache {
  constructor(
    dataDir: string,
    private readonly forced: RunnerClone["seededFrom"],
  ) {
    super(dataDir, nullLogger());
  }
  override async runnerCloneForBranch(
    barePath: string,
    branch: string,
    key: string,
    runId?: string,
  ): Promise<RunnerClone> {
    const rc = await super.runnerCloneForBranch(barePath, branch, key, runId);
    return { ...rc, seededFrom: this.forced };
  }
}

/** The reports the worker sent that carry `seeded_from_default: true`, for one run. */
function seededReports(runId: string) {
  return api.states.filter(
    (s) => s.runId === runId && s.body.seeded_from_default === true,
  );
}

/** A RunRunner over a forced-seed git and the harness client/gitlab, matching runnerWith's
 *  options. */
function runnerWithForcedSeed(
  seed: RunnerClone["seededFrom"],
  factory: ExecutorFactory,
): RunRunner {
  const { gitlab } = fakeGitlab();
  return new RunRunner(
    client,
    new ForcedSeedGit(fx.dataDir, seed),
    factory,
    nullLogger(),
    20,
    undefined,
    { pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, gitlab },
  );
}

describe("RunRunner — seeded_from_default milestone-reset signal (PRD #628 M4)", () => {
  it("emits exactly one seeded_from_default=true report on a default reseed, and NEVER on an iteration report", async () => {
    // A cross-worker RE-CLAIM (session_id set = the run ran before) reseeds from the default
    // branch with the REAL git, so seededFrom is genuinely "default" here (no forcing) — the
    // end-to-end anchor for the resume-that-lost-its-tree case the reset signal exists for.
    const { gitlab } = fakeGitlab();
    // The executor drives one iteration report (which must NOT carry the field) then ends.
    const factory: ExecutorFactory = () => ({
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          await ctx.reportIteration?.(1);
          return { branch: ctx.branch };
        },
      },
    });
    const claim = gitlabClaim(628, { session_id: "sess-628-prior" });
    const runner = new RunRunner(client, git, factory, nullLogger(), 20, undefined, {
      pollMs: 5,
      planApprovalTimeoutMs: 0,
      questionTimeoutMs: 600,
      gitlab,
    });
    await runner.execute(claim);

    const fired = seededReports(claim.run_id);
    assert.strictEqual(
      fired.length,
      1,
      `exactly one seeded_from_default report expected, got ${fired.length}`,
    );
    assert.strictEqual(fired[0]!.body.status, "running", "the signal rides a running report");
    assert.strictEqual(
      fired[0]!.body.iteration_count,
      undefined,
      "the one-shot signal is a DEDICATED report, not folded into an iteration heartbeat",
    );

    // The discriminating half: no report that IS an iteration heartbeat (iteration_count
    // set) may carry the field — that is the per-heartbeat re-clear the one-shot forbids.
    const iterWithField = api.states.filter(
      (s) =>
        s.runId === claim.run_id &&
        s.body.iteration_count !== undefined &&
        s.body.seeded_from_default === true,
    );
    assert.strictEqual(
      iterWithField.length,
      0,
      "an iteration report must never carry seeded_from_default (it would wipe live progress)",
    );
    // And the iteration report we drove was actually sent (so the negative above is not vacuous).
    assert.ok(
      api.states.some(
        (s) => s.runId === claim.run_id && s.body.iteration_count === 1,
      ),
      "the executor's iteration report was recorded",
    );
  });

  it("does NOT emit seeded_from_default when the reseed recovered a checkpoint (cross-worker tree recovery)", async () => {
    const factory: ExecutorFactory = () => ({
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => ({ branch: ctx.branch }),
      },
    });
    const claim = gitlabClaim(629, {});
    await runnerWithForcedSeed("checkpoint", factory).execute(claim);
    assert.strictEqual(
      seededReports(claim.run_id).length,
      0,
      "a checkpoint recovery preserves milestones — the reset signal must NOT fire",
    );
  });

  it("does NOT emit seeded_from_default when the reseed recovered the tracking ref", async () => {
    const factory: ExecutorFactory = () => ({
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => ({ branch: ctx.branch }),
      },
    });
    const claim = gitlabClaim(630, {});
    await runnerWithForcedSeed("tracking", factory).execute(claim);
    assert.strictEqual(
      seededReports(claim.run_id).length,
      0,
      "a tracking-ref recovery preserves milestones — the reset signal must NOT fire",
    );
  });

  it("does NOT emit seeded_from_default on a brand-new first attempt (no prior session)", async () => {
    // A fresh first attempt reseeds from default too, but has no prior milestones to clear;
    // firing here would add a spurious extra run-start report (it regressed runner-push-mr's
    // status-sequence assertions). Gated on claim.session_id != null, so it stays silent.
    const factory: ExecutorFactory = () => ({
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => ({ branch: ctx.branch }),
      },
    });
    const claim = gitlabClaim(631, {}); // no session_id ⇒ first attempt
    await runnerWithForcedSeed("default", factory).execute(claim);
    assert.strictEqual(
      seededReports(claim.run_id).length,
      0,
      "a first attempt (no session_id) has nothing to clear — the reset signal must NOT fire",
    );
  });
});
