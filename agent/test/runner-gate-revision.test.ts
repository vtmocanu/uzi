// PRD #1795 M3: plan-gate verdicts bound to the gate revision, through the REAL RunRunner gate
// (gatePlan's report, ACK and steering) against a FakeApi that allocates revisions and stamps
// verdict rows like the api (bound while a gate is visible, unbound otherwise). B1-B5 of the PRD
// on a single claim; the reclaim cases (id reuse, adoption, refusals) are in
// runner-gate-revision-reclaim.test.ts.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { PlanRejectedError, type Executor, type RunContext } from "../src/executor.js";
import { CI_CONFIG_MARKER } from "../src/prompt.js";
import { CredentialSwitchSignal, type PlanVerdict } from "../src/steering.js";
import { api, fakeGitlab, git, installHarness, runner } from "./runner-harness.js";
import {
  GateExecutor,
  UUID_RE,
  feed,
  freshClaim,
  gates,
  holdGateAck,
  newApi,
  routed,
  row,
  send,
  statuses,
  until,
} from "./gate-revision-harness.js";

installHarness();

const PLAN_1 = "# PLAN v1\n- first cut";
const PLAN_2 = "# PLAN v2\n- revised";
const PLAN_3 = "# PLAN v3\n- revised again";
const UNBOUND_APPROVE_NOTICE =
  "Approval ignored — it was sent while no plan was awaiting approval; approve the plan once it is shown.";

/** Run `exec` on `claim` with a bounded approval wait, so a verdict that is never taken ends the
 *  gate with the timeout reject (a named failure) instead of hanging the file. */
function start(exec: GateExecutor, claim: ReturnType<typeof freshClaim>, approvalMs = 4_000): Promise<void> {
  const { gitlab } = fakeGitlab();
  return runner(exec, gitlab, undefined, { planApprovalTimeoutMs: approvalMs }).execute(claim);
}

describe("B1: a verdict routed after persistence but before the ACK is taken (PRD #1795)", () => {
  for (const kind of ["approve", "reject", "revise"] as const) {
    it(`a ${kind} of the revised plan, created while its report's ACK is in flight, acts on it`, async () => {
      newApi();
      const claim = freshClaim();
      const exec = new GateExecutor([PLAN_1, PLAN_2, PLAN_3]);
      const runId = claim.run_id;
      // Round 1: revise the first plan (bound to revision 1 by the api once it is shown).
      api.onState(runId, (b) => {
        if (b.status === "awaiting_approval" && b.plan_md === PLAN_1) send(runId, row("revise_plan", "tighten it"));
      });
      // Round 2: persist the revised plan, then hold its ACK. A verdict created now is bound by
      // the api to revision 2 and routed by the worker BEFORE the gate learns it is revision 2
      // (and before the revision gate bumps its epoch).
      const held = holdGateAck(runId, PLAN_2);
      const done = start(exec, claim);
      assert.ok(await until(() => gates(runId).some((g) => g.plan_md === PLAN_2)), "the revised plan was persisted");
      assert.equal(api.gateOf(runId).revision, 2);
      const verdict =
        kind === "approve" ? row("approve_plan") : kind === "reject" ? row("reject_plan", "not like this") : row("revise_plan", "one more pass");
      const [stored] = send(runId, verdict);
      assert.deepStrictEqual([stored!.gate_binding, stored!.gate_revision], ["bound", 2], "the api bound it to the persisted gate");
      await routed(runId, verdict.id);
      if (kind === "revise") api.onState(runId, (b) => {
        if (b.status === "awaiting_approval" && b.plan_md === PLAN_3) send(runId, row("approve_plan"));
      });
      held.release();
      await done;

      const kinds = exec.verdicts.map((v) => v.kind);
      if (kind === "approve") {
        assert.deepStrictEqual(kinds, ["revise", "approve"], "the approve of plan 2 was taken");
        assert.ok(statuses(runId).includes("completed"));
      } else if (kind === "reject") {
        assert.deepStrictEqual(exec.verdicts.at(-1), { kind: "reject", reason: "not like this" });
        assert.ok(statuses(runId).includes("failed"));
      } else {
        assert.deepStrictEqual(kinds, ["revise", "revise", "approve"], "the revise of plan 2 was taken, then plan 3 approved");
        assert.deepStrictEqual(gates(runId).map((g) => g.plan_md), [PLAN_1, PLAN_2, PLAN_3]);
      }
      assert.ok(!api.isDiscarded(runId, verdict.id), "the verdict was never disposed of as stale");
    });
  }
});

describe("B2 / B3: an old-plan verdict never authorizes a later plan (PRD #1795)", () => {
  it("B2: an approve bound to plan 1, sent during the revision turn and polled after plan 2 is shown, stays stale", async () => {
    newApi();
    const claim = freshClaim();
    const runId = claim.run_id;
    const exec = new GateExecutor([PLAN_1, PLAN_2]);
    let releaseRevision!: () => void;
    exec.holdRevision(1, new Promise<void>((r) => (releaseRevision = r)));
    api.onState(runId, (b) => {
      if (b.status === "awaiting_approval" && b.plan_md === PLAN_1) send(runId, row("revise_plan", "rework"));
    });
    const done = start(exec, claim, 1_500);
    assert.ok(await until(() => exec.verdicts.length === 1), "the revise was taken");
    // The api still shows gate 1 during the revision turn, so this approve binds to revision 1.
    const [stale] = send(runId, row("approve_plan"));
    assert.equal(stale!.gate_revision, 1);
    releaseRevision();
    await done;
    assert.deepStrictEqual(exec.verdicts.map((v) => v.kind), ["revise", "reject"], "plan 2 was never approved (the gate timed out)");
    assert.ok(api.isDiscarded(runId, stale!.id), "the stale approve went to the discard lane");
    assert.ok(!api.humanPlanApproved(runId), "and never counts as the human approval");
  });

  it("B3: a declined revised report confirms nothing, so a buffered approve of plan 1 is never taken", async () => {
    newApi();
    const claim = freshClaim();
    const runId = claim.run_id;
    const exec = new GateExecutor([PLAN_1, PLAN_2]);
    let releaseRevision!: () => void;
    exec.holdRevision(1, new Promise<void>((r) => (releaseRevision = r)));
    api.onState(runId, (b) => {
      if (b.status === "awaiting_approval" && b.plan_md === PLAN_1) send(runId, row("revise_plan", "rework"));
    });
    // The revised plan's report is declined (409, applied:false).
    api.failStateWhen(runId, (b) => b.status === "awaiting_approval" && b.plan_md === PLAN_2, { runStatus: "awaiting_approval" });
    const done = start(exec, claim, 1_500);
    assert.ok(await until(() => exec.verdicts.length === 1));
    const [buffered] = send(runId, row("approve_plan"));
    assert.equal(buffered!.gate_revision, 1);
    await routed(runId, buffered!.id);
    releaseRevision();
    await done;
    assert.deepStrictEqual(exec.verdicts.map((v) => v.kind), ["revise", "reject"], "nothing approved the unseen plan");
    assert.ok(!api.humanPlanApproved(runId));
    assert.equal(api.gateOf(runId).revision, 1, "the declined report allocated nothing");
  });

  it("B3: a report persisted then its ACK lost is retried under the SAME presentation id, allocating once", async () => {
    newApi();
    const claim = freshClaim();
    const runId = claim.run_id;
    const exec = new GateExecutor([PLAN_1]);
    api.afterPersistState(runId, (b) => b.status === "awaiting_approval", "drop");
    api.onState(runId, (b) => {
      if (b.status === "awaiting_approval") send(runId, row("approve_plan"));
    });
    await start(exec, claim);
    const reports = gates(runId);
    assert.equal(reports.length, 2, "the lost ACK was retried once");
    assert.match(reports[0]!.presentation_id ?? "", UUID_RE);
    assert.equal(reports[1]!.presentation_id, reports[0]!.presentation_id, "the retry reused the id");
    assert.equal(api.gateOf(runId).revision, 1, "no double allocation");
    assert.deepStrictEqual(exec.verdicts.map((v) => v.kind), ["approve"], "the approve bound to revision 1 was taken");
    assert.ok(statuses(runId).includes("completed"));
  });
});

describe("B4: a revise stays unapplied until the revised plan's applied ACK (PRD #1795)", () => {
  it("the bound revise is applied only after the revised gate's report is answered", async () => {
    newApi();
    const claim = freshClaim();
    const runId = claim.run_id;
    const exec = new GateExecutor([PLAN_1, PLAN_2]);
    let reviseId = 0;
    api.onState(runId, (b) => {
      if (b.status === "awaiting_approval" && b.plan_md === PLAN_1) reviseId = send(runId, row("revise_plan", "rework"))[0]!.id;
    });
    const held = holdGateAck(runId, PLAN_2);
    const done = start(exec, claim);
    assert.ok(await until(() => gates(runId).some((g) => g.plan_md === PLAN_2)));
    await new Promise((r) => setTimeout(r, 60));
    assert.ok(!api.isApplied(runId, reviseId), "persisted but unanswered: the revise is still replayable");
    api.onState(runId, () => undefined);
    held.release();
    assert.ok(await until(() => api.isApplied(runId, reviseId)), "applied once the revised gate is confirmed");
    send(runId, row("approve_plan"));
    await done;
    assert.deepStrictEqual(exec.verdicts.map((v) => v.kind), ["revise", "approve"]);
  });
});

describe("B5: verdicts sent while no gate is visible (PRD #1795, A2)", () => {
  it("an approve sent during initial planning is disposed of and never approves the first gate", async () => {
    newApi();
    const claim = freshClaim();
    const runId = claim.run_id;
    const [early] = send(runId, row("approve_plan"));
    assert.equal(early!.gate_binding, "unbound");
    await start(new GateExecutor([PLAN_1]), claim, 1_500);
    assert.ok(api.isDiscarded(runId, early!.id), "disposed of through the discard lane");
    assert.ok(feed(runId).includes(UNBOUND_APPROVE_NOTICE), "with a notice");
    assert.ok(!statuses(runId).includes("completed"), "the first gate was not approved");
    assert.ok(!api.humanPlanApproved(runId));
  });

  it("a reject sent during initial planning still stops the run at its first gate", async () => {
    newApi();
    const claim = freshClaim();
    const runId = claim.run_id;
    const [early] = send(runId, row("reject_plan", "wrong issue"));
    assert.equal(early!.gate_binding, "unbound");
    const exec = new GateExecutor([PLAN_1]);
    await start(exec, claim);
    assert.deepStrictEqual(exec.verdicts, [{ kind: "reject", reason: "wrong issue" }]);
    assert.equal(gates(runId).length, 1, "stopped at its first gate");
    assert.ok(statuses(runId).includes("failed"));
  });

  it("revise feedback sent before the first gate is taken at it", async () => {
    newApi();
    const claim = freshClaim();
    const runId = claim.run_id;
    const [early] = send(runId, row("revise_plan", "include the migration"));
    assert.equal(early!.gate_binding, "unbound");
    const exec = new GateExecutor([PLAN_1, PLAN_2]);
    api.onState(runId, (b) => {
      if (b.status === "awaiting_approval" && b.plan_md === PLAN_2) send(runId, row("approve_plan"));
    });
    await start(exec, claim);
    assert.deepStrictEqual(exec.verdicts[0], { kind: "revise", feedback: "include the migration", inputId: early!.id });
    assert.deepStrictEqual(exec.verdicts.map((v) => v.kind), ["revise", "approve"]);
  });
});

describe("the presentation fields ride the report only when the api advertised them (PRD #1795 D4)", () => {
  it("with gate_revision_v1 each gate mints its own id and confirms the ACK's revision", async () => {
    newApi();
    const claim = freshClaim();
    const runId = claim.run_id;
    api.onState(runId, (b) => {
      if (b.status !== "awaiting_approval") return;
      send(runId, row(b.plan_md === PLAN_1 ? "revise_plan" : "approve_plan", b.plan_md === PLAN_1 ? "again" : null));
    });
    await start(new GateExecutor([PLAN_1, PLAN_2]), claim);
    const [g1, g2] = gates(runId);
    assert.match(g1!.presentation_id ?? "", UUID_RE);
    assert.match(g2!.presentation_id ?? "", UUID_RE);
    assert.notEqual(g1!.presentation_id, g2!.presentation_id, "a revision gate mints a fresh id");
    assert.equal(g1!.adopt_gate_revision, undefined);
    assert.equal(api.gateOf(runId).revision, 2);
  });

  it("without it the report carries neither field, and bound verdicts still match the ACK's revision", async () => {
    newApi({ negotiated: false });
    const claim = freshClaim();
    const runId = claim.run_id;
    api.onState(runId, (b) => {
      if (b.status === "awaiting_approval") send(runId, row("approve_plan"));
    });
    const exec = new GateExecutor([PLAN_1]);
    await start(exec, claim);
    const [g] = gates(runId);
    assert.ok(!("presentation_id" in g!) && !("adopt_gate_revision" in g!), "no new field reaches an api that did not advertise it");
    assert.deepStrictEqual(exec.verdicts.map((v) => v.kind), ["approve"]);
  });

  it("an older api (no revisions, legacy rows) keeps today's epoch gate", async () => {
    newApi({ negotiated: false });
    api.gateRevisions = false;
    api.stampGateBindings = false;
    const claim = freshClaim();
    const runId = claim.run_id;
    api.onState(runId, (b) => {
      if (b.status !== "awaiting_approval") return;
      send(runId, row(b.plan_md === PLAN_1 ? "revise_plan" : "approve_plan", b.plan_md === PLAN_1 ? "again" : null));
    });
    const exec = new GateExecutor([PLAN_1, PLAN_2]);
    await start(exec, claim);
    assert.deepStrictEqual(exec.verdicts.map((v) => v.kind), ["revise", "approve"]);
    assert.ok(api.inputRows(runId).every((r) => r.gate_binding === undefined && r.gate_revision === undefined));
  });
});

describe("only this gate's own applied awaiting_approval ACK confirms a revision (PRD #1795 decision 6)", () => {
  for (const c of [
    { name: "a 409 (not applied) that still carries gate_revision", httpStatus: 409, runStatus: "awaiting_approval" },
    { name: "an applied ACK whose run is not awaiting_approval", httpStatus: 200, runStatus: "running" },
  ]) {
    it(`${c.name} confirms nothing: an approve bound to that revision is not taken`, async () => {
      newApi();
      const claim = freshClaim();
      const runId = claim.run_id;
      let bound: number | undefined;
      api.answerStateOnce(
        runId,
        (b) => {
          if (b.status !== "awaiting_approval") return false;
          // Created as the report arrives, bound to the revision the answer names.
          bound = send(runId, row("approve_plan", null, { gate_binding: "bound", gate_revision: 1 }))[0]!.id;
          return true;
        },
        { httpStatus: c.httpStatus, runStatus: c.runStatus, gateRevision: 1 },
      );
      const exec = new GateExecutor([PLAN_1]);
      await start(exec, claim, 1_500);
      assert.ok(bound !== undefined, "the gate report was answered");
      assert.deepStrictEqual(exec.verdicts.map((v) => v.kind), ["reject"], "the gate timed out: nothing approved it");
      assert.ok(api.isDiscarded(runId, bound!), "the bound approve was never taken (disposed of when the gate closed)");
      assert.ok(!api.humanPlanApproved(runId));
    });
  }
});

/** Plans, then waits at the gate; a credential switch at the gate that GIVES UP re-runs the SAME gate
 *  on the old token, exactly as the SDK executor's runThroughSwitch does. */
class SwitchAtGateExecutor implements Executor {
  readonly verdicts: PlanVerdict[] = [];
  readonly outcomes: string[] = [];
  constructor(private readonly plan: string) {}

  async run(ctx: RunContext): Promise<{ branch: string; switchReleased?: boolean }> {
    for (;;) {
      let v: PlanVerdict;
      try {
        v = await ctx.gatePlan!(this.plan);
      } catch (err) {
        if (!(err instanceof CredentialSwitchSignal)) throw err;
        const outcome = await ctx.attemptCredentialSwitch!();
        this.outcomes.push(outcome);
        if (outcome === "released") return { branch: ctx.branch, switchReleased: true };
        if (outcome !== "gave_up") throw err;
        continue;
      }
      this.verdicts.push(v);
      if (v.kind === "approve") return { branch: ctx.branch };
      if (v.kind === "reject") throw new PlanRejectedError(v.reason);
      throw new Error("run cancelled");
    }
  }
}

describe("a gate re-run in the same claim starts unconfirmed (PRD #1795 decision 6)", () => {
  it("a credential-switch give-up re-runs the gate; its declined report confirms nothing, so an approve bound to the first gate is not taken", async () => {
    newApi();
    const claim = freshClaim();
    const runId = claim.run_id;
    let bound: number | undefined;
    api.onState(runId, (b) => {
      if (b.status !== "awaiting_approval" || bound !== undefined) return;
      // Gate 1 is shown (and confirmed by its ACK). The re-run's report will be declined; an approve
      // bound to revision 1 is created exactly as it arrives.
      api.failStateWhen(
        runId,
        (r) => {
          if (r.status !== "awaiting_approval") return false;
          bound = send(runId, row("approve_plan", null, { gate_binding: "bound", gate_revision: 1 }))[0]!.id;
          return true;
        },
        { runStatus: "awaiting_approval" },
      );
      // A capture that never verifies (the dirty tree's WIP commit fails): the switch gives up.
      git.worktreeStatus = (async () => ["M src/impl.ts"]) as typeof git.worktreeStatus;
      git.commitWipMarker = (async () => false) as typeof git.commitWipMarker;
      api.requestCredentialSwitch(runId, 1);
    });
    const exec = new SwitchAtGateExecutor(PLAN_1);
    const { gitlab } = fakeGitlab();
    await runner(exec, gitlab, undefined, { planApprovalTimeoutMs: 1_500, recoveryRetryMs: 1 }).execute(claim);
    assert.deepStrictEqual(exec.outcomes, ["gave_up"], "the switch gave up and the gate re-ran");
    assert.ok(statuses(runId).includes("credential_switch_failed"));
    assert.ok(bound !== undefined, "the re-run gate's report was declined");
    assert.equal(api.gateOf(runId).revision, 1, "the declined report allocated nothing");
    assert.deepStrictEqual(exec.verdicts.map((v) => v.kind), ["reject"], "the re-run gate timed out: nothing approved it");
    assert.ok(api.isDiscarded(runId, bound!), "the approve bound to the first gate was never taken (disposed of when the gate closed)");
    assert.ok(!api.humanPlanApproved(runId));
  });
});

describe("bound/unbound handling follows whether the gate waits on a human (PRD #1795 decision 9)", () => {
  const CI_PLAN = `${CI_CONFIG_MARKER}\nEdit .gitlab-ci.yml to add the missing job`;

  it("a human-gated claim disposes of an unbound approve on arrival, before any gate is entered", async () => {
    newApi();
    const claim = freshClaim();
    const runId = claim.run_id;
    let release!: () => void;
    const exec = new GateExecutor([PLAN_1], { beforeGate: new Promise<void>((r) => (release = r)) });
    api.onState(runId, (b) => {
      if (b.status === "awaiting_approval") send(runId, row("approve_plan"));
    });
    const done = start(exec, claim, 1_500);
    try {
      const [early] = send(runId, row("approve_plan"));
      assert.equal(early!.gate_binding, "unbound");
      assert.ok(await until(() => api.isDiscarded(runId, early!.id), 2_000), "disposed of on arrival");
      assert.equal(exec.gateEntered, false, "while the plan was still being written");
    } finally {
      // Released even on a failed assertion, so the held runner never outlives the test.
      release();
      await done;
    }
    assert.deepStrictEqual(exec.verdicts.map((v) => v.kind), ["approve"], "the approve of the shown gate was taken");
  });

  it("an auto-approve ci_fix claim whose CI-config plan forces a human gate disposes of an unbound approve at that gate", async () => {
    newApi();
    const claim = freshClaim(1, { kind: "ci_fix", auto_approve: true });
    const runId = claim.run_id;
    let release!: () => void;
    const exec = new GateExecutor([CI_PLAN], { beforeGate: new Promise<void>((r) => (release = r)) });
    const done = start(exec, claim, 1_500);
    let early: ReturnType<typeof send>[number] | undefined;
    try {
      [early] = send(runId, row("approve_plan"));
      assert.equal(early!.gate_binding, "unbound");
      await routed(runId, early!.id);
      assert.ok(!api.isDiscarded(runId, early!.id), "an auto-approve claim reads it as legacy until a gate waits on a human");
    } finally {
      release();
      await done;
    }
    assert.equal(gates(runId).length, 1, "the CI-config plan parked for a human");
    assert.ok(api.isDiscarded(runId, early!.id), "the unbound approve was disposed of at the forced gate");
    assert.ok(feed(runId).includes(UNBOUND_APPROVE_NOTICE));
    assert.deepStrictEqual(exec.verdicts.map((v) => v.kind), ["reject"], "the gate timed out: the unbound approve never approved it");
    assert.ok(!api.humanPlanApproved(runId));
  });

  it("an autopilot claim never disposes of an unbound approve: its gate does not wait on a verdict", async () => {
    newApi();
    const claim = freshClaim(1, { auto_approve: true });
    const runId = claim.run_id;
    let release!: () => void;
    const exec = new GateExecutor([PLAN_1], { beforeGate: new Promise<void>((r) => (release = r)) });
    const done = start(exec, claim, 1_500);
    let early: ReturnType<typeof send>[number] | undefined;
    try {
      [early] = send(runId, row("approve_plan"));
      assert.equal(early!.gate_binding, "unbound");
      await routed(runId, early!.id);
    } finally {
      release();
      await done;
    }
    assert.deepStrictEqual(exec.verdicts.map((v) => v.kind), ["approve"], "auto-approved");
    assert.equal(gates(runId).length, 0, "no human gate");
    assert.ok(!api.isDiscarded(runId, early!.id), "the unbound approve was not disposed of");
    assert.ok(!feed(runId).includes(UNBOUND_APPROVE_NOTICE));
    assert.ok(statuses(runId).includes("completed"));
  });
});
