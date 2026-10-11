import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";

import { SdkExecutor } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import { forceIncompleteHomeHelper } from "./forced-home-helper.js";
import { scanRunProcesses, reapRunProcesses } from "../src/run-procs.js";
import { TOKEN, api, baseUrl, client, installHarness } from "./runner-harness.js";
import {
  PLAN_V1,
  SID,
  FEEDBACK,
  SELECTION,
  SELECTION_STATUS,
  DEFAULT_SELECTION_STATUS,
  STALE_APPROVE_NOTICE,
  STALE_REJECT_NOTICE,
  REPLAY_STALE_VERDICT_NOTICE,
  revisedPlan,
  tick,
  until,
  scenario,
  assertRevisedOnResume,
  resumeAndApprove,
  releaseAtGate,
} from "./runner-gate-verdict-interruption-fixture.js";

installHarness();

describe("#2230 scenario failure diagnostics", () => {
  it("an unlabelled failure retains terminal reasons, receipt/state order and handler exceptions", async (t) => {
    const output: string[] = [];
    t.mock.method(console, "error", (line: string) => output.push(line));
    const failure = new Error("intentional scenario assertion failure");
    await assert.rejects(scenario(async (s) => {
      const claim = s.claim();
      client.protocolFeatures = ["claim_generation_fence"];
      const [row] = s.send(s.input("revise_plan", "diagnostic input body must be omitted"));
      await client.ackInputs(s.runId, [row!.id], claim.claim_generation!);
      api.onState(s.runId, () => { throw new Error("intentional FakeApi handler failure"); });
      const response = await fetch(`${baseUrl}/api/worker/runs/${s.runId}/state`, {
        method: "POST",
        headers: { Authorization: `Bearer ${TOKEN}`, "Content-Type": "application/json" },
        body: JSON.stringify({ status: "running", claim_generation: claim.claim_generation }),
      });
      assert.equal(response.status, 500);
      await response.text();
      api.onState(s.runId, () => {});
      await client.reportState(s.runId, { status: "failed", claim_generation: claim.claim_generation, failure_reason: "diagnostic terminal reason", fail_origin: "diagnostic_origin" });
      throw failure;
    }), (error: unknown) => error === failure);
    assert.equal(output.length, 1, "every scenario failure emits one diagnostic before cleanup");
    const diagnostic = JSON.parse(output[0]!.slice("#1604 scenario failure: ".length));
    assert.equal(diagnostic.handlerExceptions.total, 1, "the HTTP 500 retained its handler exception");
    const reports = diagnostic.generationReports.items[0];
    assert.deepEqual(reports.statuses.items, ["running", "failed"]);
    assert.deepEqual(reports.failedReports.items, [{ failure_reason: "diagnostic terminal reason", fail_origin: "diagnostic_origin" }]);
    assert.deepEqual(diagnostic.timeline.items.map((entry: { type: string }) => entry.type), ["receipt_call", "receipt_reply", "state", "state"]);
    assert.equal(diagnostic.handlerExceptions.items[0].method, "POST");
    assert.match(diagnostic.handlerExceptions.items[0].path, /\/state$/);
    assert.match(diagnostic.handlerExceptions.items[0].error, /intentional FakeApi handler failure/);
    assert.ok(!output[0]!.includes("diagnostic input body must be omitted"));
  });

  it("a successful scenario emits no failure diagnostic", async (t) => {
    const output: string[] = [];
    t.mock.method(console, "error", (line: string) => output.push(line));
    await scenario(async (s) => {
      await client.reportState(s.runId, { status: "completed" });
    });
    assert.deepEqual(output, []);
  });
});

describe("#2230 scripted model process isolation", () => {
  it("a process-free gate scenario completes when an unrelated proc scan is incomplete", async (t) => {
    const helper = forceIncompleteHomeHelper(t);
    await scenario(async (s) => {
      helper.enabled = true;
      const control = new SdkExecutor(nullLogger(), s.home, {
        queryFn: s.model.queryFn(),
        runProcesses: { scan: scanRunProcesses, reap: reapRunProcesses },
      });
      assert.equal((await control.reapAttributedProcesses()).complete, false, "explicit real operations still fail closed");
      assert.equal(helper.calls, 1, "the control used the actual HOME helper seam");
      helper.enabled = false;
      helper.calls = 0;
      const block = s.model.block("implement");
      const flight = await s.toFirstGate();
      s.send(s.input("approve_plan"));
      await block.entered;
      helper.enabled = true;
      block.release();
      await s.finish(flight);
      assert.ok(s.statuses(flight).includes("completed"), JSON.stringify(s.states(flight)));
      assert.equal(helper.calls, 0, "the preloaded SDK default never scanned live host processes");
    });
  });
});

describe("#2230 binary checkpoint publication in FakeApi", () => {
  it("a valid Git pack gets the missing-route response without a JSON handler exception", async () => {
    const header = Buffer.alloc(12);
    header.write("PACK");
    header.writeUInt32BE(2, 4); // Git pack v2, zero objects, followed by its SHA-1 trailer.
    const pack = Buffer.concat([header, createHash("sha1").update(header).digest()]);
    const publishPath = "/api/worker/runs/binary-pack-fixture/publish";
    const response = await fetch(baseUrl + publishPath, {
      method: "POST",
      headers: { Authorization: `Bearer ${TOKEN}`, "Content-Type": "application/octet-stream" },
      body: pack,
    });
    assert.equal(response.status, 404, "the fake does not claim it persisted a checkpoint");
    assert.deepEqual(await response.json(), { error: "not found", path: publishPath });
    assert.deepEqual(api.handlerExceptions, [], "binary routing did not throw a JSON decode error");
  });
});

/** Assert a resumed claim revised the submitted plan with `feedback` instead of re-presenting it. */
describe("#1604 — a revise interrupted mid-revision turn is replayed and revises the submitted plan (c)", () => {
  for (const session of ["kept", "lost", "none"] as const) {
    const label = { kept: "resume_continued (session kept)", lost: "worker-side resume_lineage_break", none: "the no-session fixture claim" }[session];
    it(`worker shutdown mid-revision, reclaimed with ${label}`, () =>
      scenario(async (s) => {
        const first = await s.toFirstGate();
        const block = s.model.block("revise");
        const [revise] = s.send(s.input("revise_plan", FEEDBACK));
        await block.entered;
        await tick(60); // several poll ticks: on base the APPLIED goes out here
        await s.shutdown(first);
        assert.ok(!s.statuses(first).includes("failed"), "a shutdown never fails the run");
        assert.equal(api.isApplied(s.runId, revise!.id), false, "the revise stays unapplied while its revised plan is unpersisted");

        const resumed = s.start(s.resumeClaim(session));
        assert.ok(await until(() => s.gates(resumed).length >= 1 || resumed.finished), "the resumed claim reached a gate");
        const gateAt = s.gateAt(s.gates(resumed)[0]?.plan_md ?? "\u0000", resumed.timelineFrom);
        s.send(s.input("approve_plan"));
        await s.finish(resumed);

        if (session === "lost") assert.ok(s.texts(resumed).some((t) => t.includes("its earlier session could not be found")), "lineage break observed");
        assertRevisedOnResume(s, resumed, FEEDBACK, session);
        assert.equal(s.rowsOf("revise_plan").length, 1, "the replay created no new revise row");
        assert.ok(s.acks(revise!.id, resumed) >= 1, "the resumed claim read the same revise row again");
        const appliedAt = s.appliedAt(revise!.id, resumed.timelineFrom);
        assert.ok(appliedAt > gateAt && gateAt >= 0, "the replayed revise is applied only after its revised plan was persisted");
        assert.ok(s.statuses(resumed).includes("completed"), `the run completed after approving the revised plan: ${s.statuses(resumed).join(",")}`);
        assert.equal(s.model.count("implement", resumed.turnFrom), 1, "one implement turn, on the revised plan");
      }));
  }
});

/** Start a resumed claim, approve the first plan it offers, and let it end (bounded). Returns the
 *  flight and the timeline index of its first gate. */
describe("#1604 — (a) a verdict still unACKed when a switch releases the claim is replayed on the reclaim", () => {
  it("revise: the reclaim revises the submitted plan instead of re-presenting it", () =>
    scenario(async (s) => {
      const { first, row } = await releaseAtGate(s, () => s.send(s.input("revise_plan", FEEDBACK))[0]!, "unacked");
      assert.ok(s.statuses(first).includes("credential_switch"), `the switch released the claim: ${s.statuses(first).join(",")}`);
      assert.equal(api.isAcked(s.runId, row.id), false, "the released claim never ACKed the revise");
      const { flight, gateAt } = await resumeAndApprove(s, s.resumeClaim("kept"));
      assertRevisedOnResume(s, flight, FEEDBACK, "kept");
      assert.ok(s.appliedAt(row.id, flight.timelineFrom) > gateAt, "applied only after the revised plan was persisted");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }, {}, "(a) unACKed revise: reclaim revises the submitted plan"));

  it("approve: the reclaim offers the submitted plan and the replayed approve (and its selection) applies", () =>
    scenario(async (s) => {
      const { first, row } = await releaseAtGate(s, () => s.send(s.input("approve_plan", SELECTION))[0]!, "unacked");
      assert.ok(s.statuses(first).includes("credential_switch"), s.statuses(first).join(","));
      const flight = s.start(s.resumeClaim("kept"));
      await s.finish(flight);
      assert.equal(s.gates(flight)[0]?.plan_md, PLAN_V1, "the submitted plan is offered");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
      assert.ok(s.texts(flight).includes(SELECTION_STATUS), `the approve's agent selection survived: ${s.texts(flight).join(" | ")}`);
      assert.equal(s.model.count("implement"), 1, "one implement turn");
      assert.equal(s.model.count("revise"), 0, "no revision");
      assert.ok(api.isApplied(s.runId, row.id), "the replayed approve is applied");
    }));

  it("reject: the reclaim reads the reject before offering the plan and fails, settling it with the transition", () =>
    scenario(async (s) => {
      const { first, row } = await releaseAtGate(s, () => s.send(s.input("reject_plan", "wrong approach"))[0]!, "unacked");
      assert.ok(s.statuses(first).includes("credential_switch"), s.statuses(first).join(","));
      const flight = s.start(s.resumeClaim("kept"));
      await s.finish(flight);
      const failed = s.states(flight).find((b) => b.status === "failed");
      assert.equal(failed?.failure_reason, "wrong approach", `the run failed with the reject reason: ${s.statuses(flight).join(",")}`);
      assert.equal(s.model.count("implement"), 0, "nothing was implemented");
      assert.equal(s.gates(flight).length, 0, "the plan is never offered once the reject is read");
      assert.ok(api.isApplied(s.runId, row.id), "the reject is applied with the plan_rejected failed transition");
    }));

  for (const verdict of ["approve_plan", "reject_plan"] as const) {
    it(`D5: a ${verdict} applied at a resumed gate gets one status line naming the verdict`, () =>
      scenario(async (s) => {
        await releaseAtGate(s, () => s.send(s.input(verdict, verdict === "approve_plan" ? SELECTION : "wrong approach"))[0]!, "unacked");
        const flight = s.start(s.resumeClaim("kept"));
        await s.finish(flight);
        const word = verdict === "approve_plan" ? /approv/i : /reject/i;
        const lines = s.texts(flight).filter((t) => word.test(t) && t !== STALE_APPROVE_NOTICE && t !== STALE_REJECT_NOTICE);
        assert.equal(lines.length, 1, `one status line names the verdict: ${JSON.stringify(s.texts(flight))}`);
      }));
  }
});

describe("#1604 — (b) a verdict ACKed and routed whose APPLIED is refused by a pending switch", () => {
  it("revise: the revised plan is persisted and the revise applied under switch_pending before the release; the reclaim shows it with no second revision", () =>
    scenario(async (s) => {
      const { first, row } = await releaseAtGate(s, () => s.send(s.input("revise_plan", FEEDBACK))[0]!, "acked");
      const v2At = s.gateAt(revisedPlan(1));
      const releasedAt = s.stateAt("credential_switch");
      assert.ok(v2At >= 0 && releasedAt > v2At, `the revised plan was persisted before the release: ${s.statuses(first).join(",")}`);
      const appliedAt = s.appliedAt(row.id);
      assert.ok(appliedAt > v2At && appliedAt < releasedAt, "the revise was applied after persistence, under switch_pending, before the release");
      const { flight } = await resumeAndApprove(s, s.resumeClaim("kept"));
      assert.equal(s.gates(flight)[0]?.plan_md, revisedPlan(1), "the reclaim offers the revised plan");
      assert.equal(s.model.count("revise", flight.turnFrom), 0, "no second revision");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }));

  it("approve: the refused APPLIED leaves it to replay; the reclaim applies it with its selection", () =>
    scenario(async (s) => {
      const { first, row } = await releaseAtGate(s, () => s.send(s.input("approve_plan", SELECTION))[0]!, "acked");
      assert.ok(!s.statuses(first).includes("failed") && !s.statuses(first).includes("completed"), s.statuses(first).join(","));
      assert.equal(api.isApplied(s.runId, row.id), false, "the refused approve stays unapplied");
      const flight = s.start(s.resumeClaim("kept"));
      await s.finish(flight);
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
      assert.ok(s.texts(flight).includes(SELECTION_STATUS), s.texts(flight).join(" | "));
      assert.equal(s.model.count("implement"), 1, "exactly one implement turn across both claims");
    }));

  it("reject: the run fails and the reject is settled with the failed transition", () =>
    scenario(async (s) => {
      const { first, row } = await releaseAtGate(s, () => s.send(s.input("reject_plan", "wrong approach"))[0]!, "acked");
      const failed = s.states(first).find((b) => b.status === "failed");
      assert.equal(failed?.failure_reason, "wrong approach", s.statuses(first).join(","));
      assert.equal(s.model.count("implement"), 0, "nothing was implemented");
      assert.ok(api.isApplied(s.runId, row.id), "the reject is applied with the plan_rejected failed transition");
    }));
});

describe("#1604 — an approve interrupted before its running transition keeps its selection", () => {
  it("after routing, before APPLIED: the reclaim replays it", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      api.failInputReceipts("applied", 503);
      const [row] = s.send(s.input("approve_plan", SELECTION));
      assert.ok(await until(() => api.inputReceiptCalls.some((c) => c.kind === "applied" && c.ids.includes(row!.id))));
      await s.shutdown(first);
      api.failInputReceipts("applied", undefined);
      assert.ok(!s.statuses(first).includes("failed"), s.statuses(first).join(","));
      assert.equal(api.isApplied(s.runId, row!.id), false, "the approve stays unapplied");
      const flight = s.start(s.resumeClaim("kept"));
      await s.finish(flight);
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
      assert.ok(s.texts(flight).includes(SELECTION_STATUS), s.texts(flight).join(" | "));
      assert.equal(s.model.count("implement"), 1);
      assert.equal(s.rowsOf("approve_plan").length, 1, "no new row");
    }));

  it("after APPLIED, before the running transition: the approved claim implements with the persisted selection, no gate", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const stopDropping = api.dropStatesWhen(s.runId, () => true); // the worker dies: nothing it reports lands
      const [row] = s.send(s.input("approve_plan", SELECTION));
      assert.ok(await until(() => api.isApplied(s.runId, row!.id)), "the approve was applied");
      await s.shutdown(first);
      stopDropping();
      assert.equal(s.statuses(first).at(-1), "awaiting_approval", "no report after the gate landed");
      const flight = s.start(
        s.resumeClaim("kept", { plan_approved: true, resume_phase: "implementing", agent_selection: { source: "own", exclusions: ["reviewer"] } }),
      );
      await s.finish(flight);
      assert.equal(s.gates(flight).length, 0, "an approved resume never re-gates");
      assert.ok(s.texts(flight).includes(SELECTION_STATUS), s.texts(flight).join(" | "));
      assert.ok(!s.texts(flight).includes(DEFAULT_SELECTION_STATUS));
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }));
});

describe("#1604 — a reject interrupted before its failed report is replayed and fails the run", () => {
  // Contract change (review finding 1): this reject was sent while the plan was still being
  // written, so it is older than the plan frame the reclaim's resume_plan_at names. The first claim
  // takes it at its first gate (epoch 0), but a reclaim cannot tell it from a reject aimed at a
  // superseded plan, so it is stale there: the submitted plan is re-presented, not failed.
  it("buffered before the gate waiter opens (sent before the plan existed): stale on the reclaim", () =>
    scenario(async (s) => {
      const block = s.model.block("plan");
      const first = s.start(s.claim());
      await block.entered;
      const stopDropping = api.dropStatesWhen(s.runId, (b) => b.status === "failed");
      const [row] = s.send(s.input("reject_plan", "wrong approach"));
      assert.ok(await until(() => api.isAcked(s.runId, row!.id)));
      await tick(60);
      block.release();
      await s.finish(first);
      stopDropping();
      assert.equal(s.gates(first).length, 1, "the plan was persisted before the buffered reject was taken");
      assert.ok(!s.statuses(first).includes("failed"), "the failed report never landed");
      assert.equal(api.isApplied(s.runId, row!.id), false, "the reject stays unapplied without its failed transition");
      const { flight } = await resumeAndApprove(s, s.resumeClaim("kept"));
      assert.ok(!s.statuses(flight).includes("failed"), `the stale reject did not fail the run: ${s.statuses(flight).join(",")}`);
      assert.ok(s.texts(flight).includes(REPLAY_STALE_VERDICT_NOTICE), s.texts(flight).join(" | "));
      assert.equal(s.gates(flight)[0]?.plan_md, PLAN_V1, "the submitted plan is re-presented");
      assert.ok(api.isApplied(s.runId, row!.id), "the stale reject is applied on its own");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }));

  it("taken at the gate", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const stopDropping = api.dropStatesWhen(s.runId, (b) => b.status === "failed");
      const [row] = s.send(s.input("reject_plan", "wrong approach"));
      await s.finish(first);
      stopDropping();
      assert.ok(!s.statuses(first).includes("failed"), "the failed report never landed");
      assert.equal(api.isApplied(s.runId, row!.id), false, "the reject stays unapplied without its failed transition");
      const flight = s.start(s.resumeClaim("kept"));
      await s.finish(flight);
      assert.equal(s.states(flight).find((b) => b.status === "failed")?.failure_reason, "wrong approach", s.statuses(flight).join(","));
      assert.equal(s.model.count("implement"), 0, "never an implement turn");
      assert.ok(api.isApplied(s.runId, row!.id), "settled with the failed transition");
    }));
});

describe("#1604 — an already-applied reject with no replayable input (older-worker residual)", () => {
  it("the gate re-presents the submitted plan and waits; nothing is implemented", () =>
    scenario(async (s) => {
      s.writeTranscript();
      const flight = s.start(
        s.claim({ resume_phase: "awaiting_approval", plan_md: PLAN_V1, plan_source: "agent", plan_approved: false, session_id: SID }),
      );
      assert.ok(await until(() => s.gates(flight).length >= 1 || flight.finished), s.statuses(flight).join(","));
      await tick(300);
      assert.equal(s.gates(flight)[0]?.plan_md, PLAN_V1, "the submitted plan is re-presented");
      assert.equal(flight.finished, false, "the gate keeps waiting for a verdict");
      assert.equal(s.model.turns.length, 0, "no turn ran: nothing planned, revised or implemented");
      s.send(s.input("cancel"));
      await s.finish(flight);
    }));
});
