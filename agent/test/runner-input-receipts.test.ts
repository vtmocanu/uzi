// Issue #1673: the runner holds a resume report behind the applied receipt of the input that
// caused it. When that receipt keeps failing on the active claim, the run must fail cleanly
// rather than wait forever or send the resume as if the input were applied.
import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { nullLogger } from "./helpers.js";
import { Outbox } from "../src/outbox.js";
import { StubExecutor } from "../src/executor.js";
import { api, fakeGitlab, git, gitlabClaim, input, installHarness, runner, runnerWith } from "./runner-harness.js";
import type { Executor, RunContext } from "../src/executor.js";

installHarness();

const tmpRoots: string[] = [];
after(() => {
  for (const dir of tmpRoots) fs.rmSync(dir, { recursive: true, force: true });
});

async function mkOutbox(): Promise<Outbox> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "runner-input-receipts-"));
  tmpRoots.push(dir);
  const outbox = new Outbox({
    root: path.join(dir, "outbox"),
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
  });
  await outbox.init();
  return outbox;
}

describe("RunRunner — input receipts (issue #1673)", () => {
  it("fails the run, without a resume report, when the approval's APPLIED keeps failing", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1673);
    // The owner approves once the gate is reported; that approval's applied receipt then fails.
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_approval") api.setInputs(claim.run_id, [input("approve_plan")]);
    });
    api.failInputReceipts("applied", 503);
    await runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab).execute(claim);

    const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    const statuses = states.map((s) => s.status);
    const gate = statuses.indexOf("awaiting_approval");
    assert.ok(gate >= 0, `the run reached the plan gate: ${statuses.join(",")}`);
    assert.deepStrictEqual(statuses.slice(gate + 1), ["failed"], "no resume report after the gate, then failed");
    assert.match(states.at(-1)?.failure_reason ?? "", /could not confirm 1 applied operator input/);
    assert.ok(
      api.inputReceiptCalls.filter((c) => c.runId === claim.run_id && c.kind === "applied").length >= 30,
      "the applied receipt was retried up to the active-claim bound",
    );
  });

  it("stamps the claim's generation on every receipt through a full run (strict generations)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1674, { claim_generation: 5 });
    api.strictReceiptGenerations = true;
    api.setInputClaimGeneration(claim.run_id, 5);
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_approval") api.setInputs(claim.run_id, [input("approve_plan")]);
    });
    await runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.strictEqual(statuses.at(-1), "completed", statuses.join(","));
    const receipts = api.inputReceiptCalls.filter((c) => c.runId === claim.run_id);
    assert.deepStrictEqual(receipts.map((c) => [c.kind, c.generation]), [["ack", 5], ["applied", 5]]);
  });

  it("a flight fenced by a receipt stops quietly: no terminal report and no terminal journal", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1675, { claim_generation: 5 });
    const outbox = await mkOutbox();
    api.strictReceiptGenerations = true;
    api.setInputClaimGeneration(claim.run_id, 5);
    // At the gate the claim is released server-side (e.g. superseded by a reclaim): the approval's
    // ACK comes back 409 with reason "released", which ends this flight.
    api.onState(claim.run_id, (body) => {
      if (body.status !== "awaiting_approval") return;
      api.setInputClaimGeneration(claim.run_id, 6);
      api.setInputFenceReason(claim.run_id, "released");
      api.setInputs(claim.run_id, [input("approve_plan")]);
    });
    await runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab, undefined, {
      outbox,
      outboxTerminalMaxBytes: 1 << 20,
      gapFillMax: 100,
    }).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    const gate = statuses.indexOf("awaiting_approval");
    assert.ok(gate >= 0, statuses.join(","));
    assert.deepStrictEqual(statuses.slice(gate + 1), [], "no report after the fence, terminal or otherwise");
    assert.deepStrictEqual(outbox.listPendingTerminals(), [], "no terminal journal left behind");
    assert.ok(
      !api.messages(claim.run_id).some((m) => m.kind === "error"),
      "no failure surfaced on the run feed",
    );
  });

  it("sends no non-failed report when an abort lands while the approval's APPLIED is still uncertain", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1676, { claim_generation: 5 });
    api.setInputClaimGeneration(claim.run_id, 5);
    // Every report after the gate, with whether the approval's APPLIED had been answered by then.
    const afterGate: Array<{ status: string; applied: boolean }> = [];
    let gated = false;
    api.onState(claim.run_id, (body) => {
      if (gated) {
        afterGate.push({
          status: body.status,
          applied: api.inputReceiptReplies.some((r) => r.runId === claim.run_id && r.kind === "applied"),
        });
      }
      if (body.status === "awaiting_approval") {
        gated = true;
        api.setInputs(claim.run_id, [input("approve_plan")]);
      }
    });
    // The applied reply is slow; the worker shuts down (aborting the run's controller) while it is
    // still in flight, so the resume report waiting on it must not go out while it is uncertain.
    api.delayInputReceipts("applied", 1_500);
    const r = runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab);
    const running = r.execute(claim);
    for (let i = 0; i < 500 && !api.inputReceiptCalls.some((c) => c.runId === claim.run_id && c.kind === "applied"); i++)
      await new Promise((res) => setTimeout(res, 5));
    assert.ok(api.inputReceiptCalls.some((c) => c.kind === "applied"), "the approval's APPLIED is in flight");
    r.shutdown();
    await running;

    assert.ok(gated, "the run reached the plan gate");
    assert.ok(!afterGate.some((r) => r.status === "running"), `the interrupted resume report was refused: ${JSON.stringify(afterGate)}`);
    assert.deepStrictEqual(
      afterGate.filter((r) => r.status !== "failed" && !r.applied),
      [],
      "no non-failed report went out while the approval's APPLIED was uncertain",
    );
  });

  it("a switch_pending APPLIED after routing enters the credential-switch path, never `failed`, and leaves the input to replay", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1677, { claim_generation: 5 });
    api.setInputClaimGeneration(claim.run_id, 5);
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_approval") {
        api.setInputs(claim.run_id, [input("approve_plan")]);
        // The switch becomes pending after the approval is ACKed and routed, before its APPLIED.
        api.pendSwitchAfterNextAck(claim.run_id);
      }
    });
    // An executor that gates, then behaves like the SDK loop on a switch: a CredentialSwitchSignal
    // abort of the shared controller is re-thrown for the runner's existing switch handling.
    const executor: Executor = {
      run: async (ctx: RunContext) => {
        git.worktreeStatus = (async () => []) as typeof git.worktreeStatus;
        git.fetchAgentBranch = (async () => `refs/uzi-runner/${ctx.branch}`) as typeof git.fetchAgentBranch;
        git.verifyRunnerTrackingCovers = (async () => true) as typeof git.verifyRunnerTrackingCovers;
        git.trackingTip = (async () => "cafef00dcafef00dcafef00dcafef00dcafef00d") as typeof git.trackingTip;
        git.checkpointPack = (async () => null) as typeof git.checkpointPack;
        const verdict = await ctx.gatePlan!("## Plan\n- do it", undefined);
        assert.strictEqual(verdict.kind, "approve");
        for (let i = 0; i < 500 && !ctx.signal?.aborted; i++) await new Promise((r) => setTimeout(r, 5));
        const reason = ctx.signal?.reason as Error | undefined;
        if (reason?.name === "CredentialSwitchSignal") throw reason;
        return { branch: ctx.branch, reportOnly: true, summary: "no switch surfaced" };
      },
    };
    await runnerWith(() => ({ executor }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    const gate = statuses.indexOf("awaiting_approval");
    assert.ok(gate >= 0, statuses.join(","));
    const after = statuses.slice(gate + 1);
    assert.ok(!after.includes("failed") && !after.includes("completed"), `never failed or completed: ${statuses.join(",")}`);
    assert.ok(after.includes("credential_switch") || after.includes("credential_switch_failed"),
      `the existing switch path handled it: ${statuses.join(",")}`);
    assert.ok(
      !api.inputReceiptReplies.some((r) => r.runId === claim.run_id && r.kind === "applied"),
      "the approval stays unapplied, so the next claim's GET replays it",
    );
  });

  // Issue #1604 round 4 (finding 5): a taken approve whose APPLIED a pending switch refused. The
  // server would not see the run approved, so no report but the switch's own may go out until the
  // switch resolves; a give-up re-arms the approve's APPLIED, and only then does `running` go out.
  describe("a taken approve's APPLIED refused by a pending switch (issue #1604)", () => {
    /** Wait until the fake answered the approve's APPLIED with a 409. */
    async function appliedRefused(runId: string): Promise<void> {
      for (let i = 0; i < 1000; i++) {
        if (api.timeline.some((e) => e.type === "receipt_reply" && e.runId === runId && e.kind === "applied" && e.httpStatus === 409)) return;
        await new Promise((r) => setTimeout(r, 5));
      }
      assert.fail("the approve's APPLIED was never refused");
    }

    function gatedAfterAck(runId: string): void {
      api.onState(runId, (body) => {
        if (body.status === "awaiting_approval") {
          api.setInputs(runId, [input("approve_plan")]);
          api.pendSwitchAfterNextAck(runId);
        }
      });
    }

    it("a `running` report after the refusal never reaches the server; the credential switch follows", async () => {
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(1678, { claim_generation: 5 });
      api.setInputClaimGeneration(claim.run_id, 5);
      gatedAfterAck(claim.run_id);
      let reportAttempted = false;
      const executor: Executor = {
        run: async (ctx: RunContext) => {
          git.worktreeStatus = (async () => []) as typeof git.worktreeStatus;
          git.fetchAgentBranch = (async () => `refs/uzi-runner/${ctx.branch}`) as typeof git.fetchAgentBranch;
          git.verifyRunnerTrackingCovers = (async () => true) as typeof git.verifyRunnerTrackingCovers;
          git.trackingTip = (async () => "cafef00dcafef00dcafef00dcafef00dcafef00d") as typeof git.trackingTip;
          git.checkpointPack = (async () => null) as typeof git.checkpointPack;
          const verdict = await ctx.gatePlan!("## Plan\n- do it", undefined);
          assert.strictEqual(verdict.kind, "approve");
          await appliedRefused(ctx.runId);
          // The executor reports progress after the refusal (nothing is pending in the channel any
          // more: the approve is parked), as an implement turn boundary would.
          reportAttempted = true;
          await ctx.reportIteration?.(1);
          for (let i = 0; i < 500 && !ctx.signal?.aborted; i++) await new Promise((r) => setTimeout(r, 5));
          const reason = ctx.signal?.reason as Error | undefined;
          if (reason?.name === "CredentialSwitchSignal") throw reason;
          return { branch: ctx.branch, reportOnly: true, summary: "no switch surfaced" };
        },
      };
      await runnerWith(() => ({ executor }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);

      const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
      const gate = statuses.indexOf("awaiting_approval");
      assert.ok(gate >= 0 && reportAttempted, statuses.join(","));
      const after = statuses.slice(gate + 1);
      assert.ok(!after.includes("running"), `no running report reached the server: ${statuses.join(",")}`);
      assert.ok(after.includes("credential_switch"), `the credential switch followed: ${statuses.join(",")}`);
      assert.ok(!after.includes("failed") && !after.includes("completed"), statuses.join(","));
    });

    it("a switch that gives up re-arms the approve's APPLIED, and `running` goes out only after it lands", async () => {
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(1679, { claim_generation: 5 });
      api.setInputClaimGeneration(claim.run_id, 5);
      gatedAfterAck(claim.run_id);
      let outcome: string | undefined;
      const executor: Executor = {
        run: async (ctx: RunContext) => {
          // A dirty tree whose restore point never verifies: the switch gives up and the run continues.
          let marker = false;
          git.worktreeStatus = (async () => ["M src/impl.ts"]) as typeof git.worktreeStatus;
          git.commitWipMarker = (async () => { marker = true; return true; }) as typeof git.commitWipMarker;
          git.fetchAgentBranch = (async () => `refs/uzi-runner/${ctx.branch}`) as typeof git.fetchAgentBranch;
          git.verifyRunnerTrackingCovers = (async () => false) as typeof git.verifyRunnerTrackingCovers;
          git.trackingTip = (async () => "cafef00dcafef00dcafef00dcafef00dcafef00d") as typeof git.trackingTip;
          git.checkpointPack = (async () => null) as typeof git.checkpointPack;
          git.headIsWipMarker = (async () => marker) as typeof git.headIsWipMarker;
          git.undoWipMarker = (async () => { marker = false; }) as typeof git.undoWipMarker;
          const verdict = await ctx.gatePlan!("## Plan\n- do it", undefined);
          assert.strictEqual(verdict.kind, "approve");
          await appliedRefused(ctx.runId);
          for (let i = 0; i < 500 && !ctx.signal?.aborted; i++) await new Promise((r) => setTimeout(r, 5));
          outcome = await ctx.attemptCredentialSwitch!();
          await ctx.reportIteration?.(1);
          return { branch: ctx.branch, reportOnly: true, summary: "continued after the give-up" };
        },
      };
      await runnerWith(() => ({ executor }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);

      assert.strictEqual(outcome, "gave_up");
      const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
      const gate = statuses.indexOf("awaiting_approval");
      const gaveUp = statuses.indexOf("credential_switch_failed");
      assert.ok(gate >= 0 && gaveUp > gate, statuses.join(","));
      assert.ok(!statuses.slice(gate + 1, gaveUp).includes("running"), `no running before the give-up: ${statuses.join(",")}`);
      assert.ok(statuses.slice(gaveUp + 1).includes("running"), `running after the give-up: ${statuses.join(",")}`);
      const timeline = api.timeline.filter((e) => e.runId === claim.run_id);
      const giveUpAt = timeline.findIndex((e) => e.type === "state" && e.status === "credential_switch_failed");
      const appliedAt = timeline.findIndex((e, i) => i > giveUpAt && e.type === "receipt_reply" && e.kind === "applied" && e.httpStatus === 200);
      const runningAt = timeline.findIndex((e, i) => i > giveUpAt && e.type === "state" && e.status === "running");
      assert.ok(appliedAt > giveUpAt && runningAt > appliedAt, "the approve's APPLIED is retried after the give-up and lands before `running`");
      assert.strictEqual(api.humanPlanApproved(claim.run_id), true, "the approve is applied as the approval");
    });
  });
});
