import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { nullLogger } from "./helpers.js";
import { PlanRejectedError, type Executor } from "../src/executor.js";
import { RunRunner } from "../src/runner.js";
import { type UserInput } from "../src/protocol.js";
import {
  api,
  client,
  fakeGitlab,
  git,
  gitlabClaim,
  input,
  installHarness,
  runner,
  type PlanVerdictKind,
} from "./runner-harness.js";

installHarness();

// --- PRD #41: plan revision at the approval gate -------------------------------
// The runner's gatePlan is called once per round; a `revise` verdict is returned to the
// executor (which runs a fresh plan turn), and the gate re-reports awaiting_approval,
// bumping the steering epoch AT the re-report (Decision 3) so a verdict written against
// the previous plan version goes stale. All rounds share ONE approval budget.
describe("RunRunner — plan revision at the gate (PRD #41)", () => {
  const pollTick = (ms = 5): Promise<void> =>
    new Promise((r) => setTimeout(r, ms));

  /** Once the run has posted `n` awaiting_approval reports, submit `inputs`. The epoch is
   *  bumped right after each awaiting_approval RE-report (synchronously, before the gate
   *  awaits), so observing the n-th report proves the epoch has advanced to the round-n
   *  value — inputs set here are stamped at the new (current) epoch. */
  function onGateRound(
    runId: string,
    n: number,
    inputs: UserInput[],
  ): Promise<void> {
    return (async () => {
      while (
        api.states.filter(
          (s) => s.runId === runId && s.body.status === "awaiting_approval",
        ).length < n
      ) {
        await pollTick();
      }
      api.setInputs(runId, inputs);
    })();
  }

  it("returns a revise verdict for a fresh round, then completes on the round-2 approve", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(80);
    const seen: PlanVerdictKind[] = [];
    const reviseExec: Executor = {
      run: async (ctx) => {
        const v1 = await ctx.gatePlan!("# PLAN v1");
        seen.push(v1.kind);
        assert.strictEqual(v1.kind, "revise");
        assert.strictEqual(
          v1.kind === "revise" ? v1.feedback : "",
          "tighten the scope",
        );
        const v2 = await ctx.gatePlan!("# PLAN v2");
        seen.push(v2.kind);
        if (v2.kind !== "approve")
          throw new Error(`expected approve, got ${v2.kind}`);
        return { branch: ctx.branch };
      },
    };
    api.setInputs(claim.run_id, [input("revise_plan", "tighten the scope")]);
    const round2 = onGateRound(claim.run_id, 2, [input("approve_plan")]);
    await runner(reviseExec, gitlab).execute(claim);
    await round2;

    assert.deepStrictEqual(seen, ["revise", "approve"]);
    // The gate parked at awaiting_approval TWICE — once per plan version.
    const gates = api.states
      .filter(
        (s) =>
          s.runId === claim.run_id && s.body.status === "awaiting_approval",
      )
      .map((s) => s.body);
    assert.strictEqual(gates.length, 2);
    assert.match(gates[0]!.plan_md ?? "", /PLAN v1/);
    assert.match(gates[1]!.plan_md ?? "", /PLAN v2/);
    // The run completed with an MR after the round-2 approval.
    assert.ok(
      api.states.some(
        (s) => s.runId === claim.run_id && s.body.status === "completed",
      ),
    );
    assert.strictEqual(calls.length, 1);
  });

  // PRD #122 M1 (Decision 2): the CANDIDATE milestone list is REPLACED across a
  // revision round — the later awaiting_approval body reflects the NEW list, never a
  // union or the stale round-1 list. Drives the REAL runner gatePlan (not a mock) so
  // the candidate→report threading is what is under test.
  it("replaces the candidate milestone list across a revision round", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(85);
    const round1 = [{ id: "m1", title: "first cut" }];
    const round2 = [
      { id: "a", title: "restructured" },
      { id: "b", title: "second unit" },
    ];
    const reviseExec: Executor = {
      run: async (ctx) => {
        const v1 = await ctx.gatePlan!("# PLAN v1", round1);
        assert.strictEqual(v1.kind, "revise");
        const v2 = await ctx.gatePlan!("# PLAN v2", round2);
        if (v2.kind !== "approve")
          throw new Error(`expected approve, got ${v2.kind}`);
        return { branch: ctx.branch };
      },
    };
    api.setInputs(claim.run_id, [input("revise_plan", "restructure it")]);
    const approve = onGateRound(claim.run_id, 2, [input("approve_plan")]);
    await runner(reviseExec, gitlab).execute(claim);
    await approve;

    const gates = api.states
      .filter(
        (s) =>
          s.runId === claim.run_id && s.body.status === "awaiting_approval",
      )
      .map((s) => s.body);
    assert.strictEqual(gates.length, 2);
    assert.deepStrictEqual(gates[0]!.milestones, round1);
    // The round-2 report carries the NEW list wholesale, not the round-1 list.
    assert.deepStrictEqual(gates[1]!.milestones, round2);
  });

  // The BLOCKING bug (PRD #41 Decision 3): the gate epoch must advance at the
  // awaiting_approval RE-report, NOT when a revise is taken. A revision planning turn
  // runs BETWEEN rounds; if the epoch is bumped when the revise is TAKEN, that whole
  // (minutes-long) window sits at the v2 epoch, so an approve clicked mid-revision is
  // stamped current and ACCEPTED at the v2 gate — implementing a plan v2 NO human saw.
  // This drives the REAL runner + REAL steering + REAL gatePlan (not a mocked gatePlan)
  // so it exercises the epoch machinery the sdk-executor mock bypasses. It FAILS against
  // the old settle-bump code (the mid-revision approve is accepted at v2, no stale
  // notice) and PASSES once the bump moves to the re-report.
  it("approve during the revision turn is discarded (mid-revision window)", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(83);
    const seen: PlanVerdictKind[] = [];

    // Record when the steering poller actually CONSUMES the mid-revision approve, so the
    // executor waits for it before re-gating v2 — making the window deterministic (no
    // reliance on poll/HTTP timing). The approve is consumed while the epoch is still the
    // v1 epoch, so it must be stale at the v2 gate.
    const origGetInputs = client.getInputs.bind(client);
    let approveConsumed = false;
    (
      client as unknown as { getInputs: (r: string) => Promise<UserInput[]> }
    ).getInputs = async (runId: string) => {
      const inputs = await origGetInputs(runId);
      if (inputs.some((i) => i.kind === "approve_plan")) approveConsumed = true;
      return inputs;
    };

    const reviseExec: Executor = {
      run: async (ctx) => {
        const v1 = await ctx.gatePlan!("# PLAN v1");
        seen.push(v1.kind);
        assert.strictEqual(v1.kind, "revise");
        // Mid-revision window: v1 returned revise, v2 is not yet re-reported. Inject an
        // approve of the SUPERSEDED plan now, and wait until the poller has CONSUMED it
        // (stamped at the still-current v1 epoch) before we re-gate v2.
        api.setInputs(ctx.runId, [input("approve_plan")]);
        while (!approveConsumed) await pollTick();
        // Re-gate v2: the re-report bumps the epoch, so the buffered approve is now stale.
        const v2 = await ctx.gatePlan!("# PLAN v2");
        seen.push(v2.kind);
        if (v2.kind !== "approve")
          throw new Error(`expected approve, got ${v2.kind}`);
        return { branch: ctx.branch };
      },
    };
    api.setInputs(claim.run_id, [input("revise_plan", "tighten the scope")]);
    // The FRESH v2 approve is injected only after the run RE-PARKS at the v2 gate (the 2nd
    // awaiting_approval report), so it is stamped at the new epoch and legitimately taken.
    const round2 = onGateRound(claim.run_id, 2, [input("approve_plan")]);
    try {
      await runner(reviseExec, gitlab).execute(claim);
      await round2;

      // The gate saw revise then approve — but the approve it acted on was the FRESH v2
      // one, not the discarded mid-revision one (asserted via the notice + re-park below).
      assert.deepStrictEqual(seen, ["revise", "approve"]);
      // The run RE-PARKED at v2 awaiting_approval (a 2nd report carrying the v2 plan_md) —
      // it did NOT proceed to implement on the strength of the mid-revision approve.
      const gates = api.states
        .filter(
          (s) =>
            s.runId === claim.run_id && s.body.status === "awaiting_approval",
        )
        .map((s) => s.body);
      assert.strictEqual(gates.length, 2, "the run re-parked at the v2 gate");
      assert.match(gates[1]!.plan_md ?? "", /PLAN v2/);
      // The mid-revision approve was DISCARDED with a stale feed notice (the bug's tell:
      // under the old code this notice is absent because the approve was accepted at v2).
      const texts = api
        .messages(claim.run_id)
        .filter((m) => m.kind === "status")
        .map((m) => String(m.payload.text));
      assert.ok(
        texts.some((t) => t.includes("Approval ignored")),
        texts.join("\n"),
      );
      // Only after the FRESH v2 approve did the run complete + open exactly one MR.
      assert.ok(
        api.states.some(
          (s) => s.runId === claim.run_id && s.body.status === "completed",
        ),
      );
      assert.strictEqual(calls.length, 1);
    } finally {
      (client as unknown as { getInputs: unknown }).getInputs = origGetInputs;
    }
  });

  it("discards an approve batched with a revise (written against the pre-feedback plan) and notes it on the feed", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(81);
    const reviseExec: Executor = {
      run: async (ctx) => {
        const v1 = await ctx.gatePlan!("# PLAN v1");
        assert.strictEqual(v1.kind, "revise");
        const v2 = await ctx.gatePlan!("# PLAN v2");
        if (v2.kind !== "approve")
          throw new Error(`expected approve, got ${v2.kind}`);
        return { branch: ctx.branch };
      },
    };
    // The user's approve rides the SAME batch as the revise, so it is stamped against the
    // pre-feedback plan (epoch 1). Round 2 must NOT approve on it.
    api.setInputs(claim.run_id, [
      input("revise_plan", "rethink it"),
      input("approve_plan"),
    ]);
    const round2 = onGateRound(claim.run_id, 2, [input("approve_plan")]); // a fresh, round-2 approve
    await runner(reviseExec, gitlab).execute(claim);
    await round2;

    const texts = api
      .messages(claim.run_id)
      .filter((m) => m.kind === "status")
      .map((m) => String(m.payload.text));
    assert.ok(
      texts.some((t) => t.includes("Approval ignored")),
      texts.join("\n"),
    );
    assert.ok(
      api.states.some(
        (s) => s.runId === claim.run_id && s.body.status === "completed",
      ),
    );
  });

  it("shares ONE approval budget across rounds: a revise does not reset the deadline", async () => {
    // With a tiny budget and a revision round that never approves, the run times out on
    // the SHARED deadline and fails with the timeout reason — the revise kept the clock
    // running rather than granting a fresh full budget.
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(82);
    const reviseExec: Executor = {
      run: async (ctx) => {
        const v1 = await ctx.gatePlan!("# PLAN v1");
        assert.strictEqual(v1.kind, "revise");
        const v2 = await ctx.gatePlan!("# PLAN v2"); // no approve ever comes → times out
        if (v2.kind === "reject") throw new PlanRejectedError(v2.reason);
        return { branch: ctx.branch };
      },
    };
    const r = new RunRunner(
      client,
      git,
      () => ({ executor: reviseExec }),
      nullLogger(),
      20,
      undefined,
      {
        pollMs: 5,
        planApprovalTimeoutMs: 60, // small, shared across both rounds
        gitlab,
      },
    );
    api.setInputs(claim.run_id, [input("revise_plan", "one more pass")]);
    await r.execute(claim);

    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.ok(failed, "run should fail on the shared-budget timeout");
    assert.match(failed!.body.failure_reason ?? "", /plan approval timed out/);
    assert.strictEqual(calls.length, 0, "no MR when the gate times out");
  });
});
