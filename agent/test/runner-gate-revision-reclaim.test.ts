// PRD #1795 M3: which presentation a reclaimed gate publishes, and what a refused one does, through
// the REAL RunRunner (claim handling, gatePlan, the transient-recovery park) against a FakeApi that
// holds the run's gate like the api (revision, current and historical presentation ids, the
// immutable presented snapshot) and refuses what the api refuses.
//
// Decision 7: only the no-bump first gate of an SDK same-gate reclaim keeps the persisted gate's
// identity (reusing its id, or adopting an id-less gate), re-sending the PRESENTED requirements;
// every other gate mints a fresh id. Decision 8 (A3): a refused report publishes nothing, takes no
// verdict, leaves receipts unapplied and parks (recovery_wait); the next claim re-presents.
//
// The last suite pins the exact awaiting_approval /state body with and without the api's
// gate_revision_v1 feature to fixtures/worker-state-request/, which
// api/internal/handler/worker_state_decode_test.go feeds through the api's real strict decoder.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { ClaimResponse, Milestone, StateRequest } from "../src/protocol.js";
import type { Executor } from "../src/executor.js";
import { nullLogger } from "./helpers.js";
import { api, assistant, fakeGitlab, homeDir, installHarness, resultOk, runner, runnerWith } from "./runner-harness.js";
import {
  GateExecutor,
  UUID_RE,
  feed,
  freshClaim,
  gates,
  newApi,
  row,
  send,
  statuses,
} from "./gate-revision-harness.js";

installHarness();

const PLAN = "# PERSISTED PLAN\n- the plan the human was shown";
const REVISED = "# REVISED PLAN\n- after the owner's feedback";
const MILESTONES: Milestone[] = [{ id: "m1", title: "first cut" }];
const PRESENTED = { required_capabilities: ["docker"], required_tools: ["uzi-presented-tool"], size_class: "l" };
const ID_X = "0f5c2d4e-1111-4a2b-8c3d-000000000001";
const ID_Y = "0f5c2d4e-2222-4a2b-8c3d-000000000002";
const REFUSED_STATUS_PREFIX = "the plan gate could not be re-presented";

/** A resumed claim at the plan gate on the persisted, unapproved plan: the api's gate resume
 *  fields for the run as it holds it now, unless `gate` overrides them (a claim assembled from an
 *  earlier state of the gate). */
function resumeClaim(runId: string, generation: number, gate: Partial<ClaimResponse> = {}): ClaimResponse {
  api.setInputClaimGeneration(runId, generation);
  return {
    ...freshClaim(generation),
    run_id: runId,
    plan_md: PLAN,
    plan_source: "agent",
    plan_approved: false,
    milestones: MILESTONES,
    resume_phase: "awaiting_approval",
    // Every row of these tests is created after it, so no legacy row is replay-stale.
    resume_plan_at: "2000-01-01T00:00:00.000000Z",
    session_id: null,
    ...api.gateResumeFields(runId),
    ...gate,
  };
}

function seed(runId: string, revision: number, presentationId: string | null, historical: string[] = []): void {
  api.seedGate(runId, { revision, presentationId, historical, payload: { plan_md: PLAN, milestones: MILESTONES, ...PRESENTED } });
}

/** A scripted SDK query: every turn signals done, except a revision turn, which submits REVISED. */
function sdkQuery(): SdkQueryFn {
  return (params) =>
    (async function* (): AsyncGenerator<SDKMessage> {
      let prompt = "";
      for await (const p of params.prompt) prompt += JSON.stringify(p);
      if (prompt.includes("The plan reviewer read your proposed plan")) {
        yield assistant([{ type: "tool_use", id: "r", name: "mcp__uzi__submit_plan", input: { plan_md: REVISED } }]);
      } else {
        yield assistant([
          { type: "text", text: "done implementing" },
          { type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: {} },
        ]);
      }
      yield resultOk();
    })();
}

function sdkRunner(): ReturnType<typeof runnerWith> {
  const { gitlab } = fakeGitlab();
  return runnerWith(
    () => ({ executor: new SdkExecutor(nullLogger(), homeDir, { queryFn: sdkQuery() }), homeDir }),
    gitlab,
    undefined,
    nullLogger(),
    { planApprovalTimeoutMs: 4_000, recoveryRetryMs: 1 },
  );
}

function execute(exec: Executor, claim: ClaimResponse): Promise<void> {
  const { gitlab } = fakeGitlab();
  return runner(exec, gitlab, undefined, { planApprovalTimeoutMs: 4_000, recoveryRetryMs: 1 }).execute(claim);
}

/** Approve every re-presented gate once it is shown (the api binds the approve to it). */
function approveWhenShown(runId: string): void {
  api.onState(runId, (b) => {
    if (b.status === "awaiting_approval") send(runId, row("approve_plan"));
  });
}

describe("gate presentation id choice on a reclaim (PRD #1795 decision 7)", () => {
  it("an SDK same-gate reclaim reuses the persisted id and re-sends the PRESENTED requirements, keeping revision N", async () => {
    newApi();
    const runId = freshClaim().run_id;
    seed(runId, 3, ID_X);
    approveWhenShown(runId);
    // The live requirement columns an approval's capability override may have cleared, or a fresh
    // detection of the clone, differ from what the human was shown: only the snapshot counts.
    await sdkRunner().execute(resumeClaim(runId, 2));
    const [g] = gates(runId);
    assert.equal(g!.plan_md, PLAN, "the persisted plan was re-presented");
    assert.equal(g!.presentation_id, ID_X, "under the persisted presentation id");
    assert.equal(g!.adopt_gate_revision, undefined);
    assert.deepStrictEqual(
      { required_capabilities: g!.required_capabilities, required_tools: g!.required_tools, size_class: g!.size_class },
      PRESENTED,
      "the presented requirements, not a fresh detection",
    );
    assert.deepStrictEqual(api.gateOf(runId), { revision: 3, presentationId: ID_X }, "no new revision");
    assert.deepStrictEqual(api.gateRefusals, []);
    assert.ok(statuses(runId).includes("completed"), "the approve bound to revision 3 was taken");
  });

  it("an id-less persisted gate is explicitly adopted at its revision with a fresh id", async () => {
    newApi();
    const runId = freshClaim().run_id;
    seed(runId, 2, null);
    approveWhenShown(runId);
    const claim = resumeClaim(runId, 2);
    assert.equal(claim.resume_gate_presentation_id, undefined);
    const exec = new GateExecutor([PLAN], { resumesAtGate: true });
    await execute(exec, claim);
    const [g] = gates(runId);
    assert.match(g!.presentation_id ?? "", UUID_RE);
    assert.equal(g!.adopt_gate_revision, 2);
    assert.deepStrictEqual(api.gateOf(runId), { revision: 2, presentationId: g!.presentation_id! }, "adopted, not allocated");
    assert.deepStrictEqual(exec.verdicts.map((v) => v.kind), ["approve"]);
  });

  it("a resumed revise bound to the persisted gate acts first, and the revised gate gets a fresh id", async () => {
    newApi();
    const runId = freshClaim().run_id;
    seed(runId, 2, ID_X);
    const [revise] = send(runId, row("revise_plan", "split the migration", { gate_binding: "bound", gate_revision: 2 }));
    api.onState(runId, (b) => {
      if (b.status === "awaiting_approval" && b.plan_md === REVISED) send(runId, row("approve_plan"));
    });
    await sdkRunner().execute(resumeClaim(runId, 2));
    const [g] = gates(runId);
    assert.equal(g!.plan_md, REVISED, "the submitted plan was revised, not re-presented");
    assert.match(g!.presentation_id ?? "", UUID_RE);
    assert.notEqual(g!.presentation_id, ID_X, "a resumed-revise gate mints a fresh id");
    assert.equal(g!.adopt_gate_revision, undefined);
    assert.deepStrictEqual(api.gateOf(runId), { revision: 3, presentationId: g!.presentation_id! });
    assert.ok(api.isApplied(runId, revise!.id), "the revise was settled by the revised gate");
    assert.ok(statuses(runId).includes("completed"));
  });

  it("a pending reject bound to the persisted gate stops the run before the plan is offered again", async () => {
    newApi();
    const runId = freshClaim().run_id;
    seed(runId, 4, ID_X);
    send(runId, row("reject_plan", "not this", { gate_binding: "bound", gate_revision: 4 }));
    const exec = new GateExecutor([PLAN], { resumesAtGate: true });
    await execute(exec, resumeClaim(runId, 2));
    assert.deepStrictEqual(exec.verdicts, [{ kind: "reject", reason: "not this" }]);
    assert.equal(gates(runId).length, 0, "never re-presented");
    assert.ok(statuses(runId).includes("failed"));
  });

  it("a stub/Codex re-gate of IDENTICAL text mints a new id and a new revision (text equality is not identity)", async () => {
    newApi();
    const runId = freshClaim().run_id;
    seed(runId, 2, ID_X);
    approveWhenShown(runId);
    // resumesAtGate absent: the executor never re-presents, so its first gate bumps like a revision.
    const exec = new GateExecutor([PLAN]);
    await execute(exec, resumeClaim(runId, 2));
    const [g] = gates(runId);
    assert.equal(g!.plan_md, PLAN, "the same text");
    assert.match(g!.presentation_id ?? "", UUID_RE);
    assert.notEqual(g!.presentation_id, ID_X);
    assert.equal(g!.adopt_gate_revision, undefined);
    assert.equal(api.gateOf(runId).revision, 3, "a new gate");
    assert.deepStrictEqual(exec.verdicts.map((v) => v.kind), ["approve"]);
  });
});

describe("a refused gate report is a failed gate report (PRD #1795 A3, decision 8)", () => {
  const cases: Array<{
    reason: string;
    /** Seed the api's gate; return the stale first claim's gate fields. */
    arrange: (runId: string) => Partial<ClaimResponse>;
    /** The revision the pending (older) approve was bound to. */
    pendingRevision: number;
    /** The revision the next claim's accepted re-presentation keeps. */
    accepted: { revision: number; presentationId: string };
    /** Whether that pending approve then acts on the accepted gate (the same gate re-presented). */
    pendingActs: boolean;
  }> = [
    {
      reason: "gate_presentation_historical",
      arrange: (runId) => {
        seed(runId, 3, ID_Y, [ID_X]);
        return { resume_gate_revision: 2, resume_gate_presentation_id: ID_X };
      },
      pendingRevision: 2,
      accepted: { revision: 3, presentationId: ID_Y },
      pendingActs: false,
    },
    {
      reason: "gate_presentation_conflict",
      arrange: (runId) => {
        seed(runId, 2, ID_X);
        // A claim whose presented requirements differ from the snapshot: the same id, a changed payload.
        return { resume_gate_presented: { required_capabilities: ["gpu"], required_tools: [], size_class: "s" } };
      },
      pendingRevision: 2,
      accepted: { revision: 2, presentationId: ID_X },
      pendingActs: true,
    },
    {
      reason: "gate_adoption_stale",
      arrange: (runId) => {
        seed(runId, 3, ID_Y);
        return { resume_gate_revision: 2, resume_gate_presentation_id: undefined };
      },
      pendingRevision: 2,
      accepted: { revision: 3, presentationId: ID_Y },
      pendingActs: false,
    },
  ];

  for (const c of cases) {
    it(`${c.reason}: no verdict is taken, receipts stay unapplied, the run parks, and the next claim re-presents`, async () => {
      newApi();
      const runId = freshClaim().run_id;
      const staleGate = c.arrange(runId);
      const [pending] = send(runId, row("approve_plan", null, { gate_binding: "bound", gate_revision: c.pendingRevision }));

      // Flight 1: the claim's gate data is stale; the api refuses the re-presentation.
      const first = new GateExecutor([PLAN], { resumesAtGate: true });
      await execute(first, resumeClaim(runId, 2, staleGate));
      assert.deepStrictEqual(api.gateRefusals.map((r) => [r.reason, r.generation]), [[c.reason, 2]]);
      assert.deepStrictEqual(first.verdicts, [], "no verdict was taken");
      assert.ok(!api.isApplied(runId, pending!.id) && !api.isDiscarded(runId, pending!.id), "the pending approve stays replayable");
      assert.ok(statuses(runId).includes("recovery_wait"), "parked through transient recovery");
      assert.ok(!statuses(runId).includes("failed"));
      assert.ok(
        feed(runId).some((t) => t.startsWith(`${REFUSED_STATUS_PREFIX} (${c.reason})`)),
        "the refusal is named on the feed",
      );
      assert.equal(gates(runId).length, 0, "the refused payload was never published");
      assert.equal(api.gateOf(runId).revision, c.accepted.revision, "the refusal allocated nothing");

      // Flight 2: the next claim carries the api's current gate; its re-presentation is accepted.
      const reportsBefore = gates(runId).length;
      if (!c.pendingActs) approveWhenShown(runId);
      const second = new GateExecutor([PLAN], { resumesAtGate: true });
      await execute(second, resumeClaim(runId, 3));
      const accepted = gates(runId).slice(reportsBefore);
      assert.equal(accepted.length, 1);
      assert.equal(accepted[0]!.presentation_id, c.accepted.presentationId, "re-presented under the current id");
      assert.deepStrictEqual(api.gateOf(runId), c.accepted);
      assert.equal(api.gateRefusals.length, 1, "accepted");
      assert.deepStrictEqual(second.verdicts.map((v) => v.kind), ["approve"]);
      if (c.pendingActs) assert.ok(api.isApplied(runId, pending!.id), "the approve of that same gate acts on its re-presentation");
      else assert.ok(api.isDiscarded(runId, pending!.id), "the approve of the superseded gate is stale");
      assert.ok(statuses(runId).includes("completed"));
    });
  }

  it("a refusal past the api's cap comes back failed: the worker does not park over it", async () => {
    newApi();
    api.gateRefusalMax = 1;
    const runId = freshClaim().run_id;
    seed(runId, 3, ID_Y, [ID_X]);
    // Two claims with the same stale gate data: the first refusal is counted under the cap and
    // parks; the second (another claim generation) is past it and fails the run server-side.
    await execute(new GateExecutor([PLAN], { resumesAtGate: true }), resumeClaim(runId, 2, { resume_gate_revision: 2, resume_gate_presentation_id: ID_X }));
    api.setOwnershipStatus(runId, "failed");
    const statusesBefore = statuses(runId).length;
    await execute(new GateExecutor([PLAN], { resumesAtGate: true }), resumeClaim(runId, 3, { resume_gate_revision: 2, resume_gate_presentation_id: ID_X }));
    assert.deepStrictEqual(api.gateRefusals.map((r) => r.generation), [2, 3]);
    assert.ok(!statuses(runId).slice(statusesBefore).includes("recovery_wait"), "no park reported over the failed run");
  });
});

describe("the awaiting_approval /state body an older api's strict decoder sees (PRD #1795 D4)", () => {
  const FIXTURE_DIR = new URL("../../fixtures/worker-state-request/", import.meta.url);
  const NORMALIZED_ID = "00000000-0000-4000-8000-000000000000";

  /** Capture the first awaiting_approval body a fresh claim sends, as the api received it. */
  async function capture(negotiated: boolean): Promise<Record<string, unknown>> {
    newApi({ negotiated });
    const claim = freshClaim(7);
    approveWhenShown(claim.run_id);
    await execute(new GateExecutor([PLAN], { milestones: MILESTONES }), claim);
    const body = gates(claim.run_id)[0] as StateRequest & Record<string, unknown>;
    assert.ok(body, "an awaiting_approval report was sent");
    if (negotiated) {
      assert.match(body.presentation_id ?? "", UUID_RE, "a minted presentation id");
      // The id is random per gate; the fixture pins its presence and shape, not its value.
      body.presentation_id = NORMALIZED_ID;
    }
    return body;
  }

  for (const c of [
    { name: "without gate_revision_v1 advertised", negotiated: false, file: "awaiting_approval.json" },
    { name: "with gate_revision_v1 advertised", negotiated: true, file: "awaiting_approval.gate_revision_v1.json" },
  ]) {
    it(`${c.name}: equals fixtures/worker-state-request/${c.file}`, async () => {
      const actual = await capture(c.negotiated);
      const expected = JSON.parse(fs.readFileSync(new URL(c.file, FIXTURE_DIR), "utf8")) as unknown;
      assert.deepStrictEqual(
        actual,
        expected,
        `the /state body drifted from fixtures/worker-state-request/${c.file}; a deliberate wire change is a copy of:\n${JSON.stringify(actual, null, 2)}`,
      );
      if (!c.negotiated) assert.ok(!("presentation_id" in actual) && !("adopt_gate_revision" in actual));
    });
  }
});
