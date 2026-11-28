// Issue #4806 (m2): the deferral window of a human revision, the typed routing of a receipt-refusal switch signal
// raised inside it, renewal precedence and the memory guards, driven through the real runner, a real SdkExecutor over a
// scripted provider, a real WorkerMemoryRuntime and the in-place apply route of the api fake.
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import fs from "node:fs";
import { api, client, git, installHarness } from "./runner-harness.js";
import { getEventListeners } from "node:events";
import { RunRunner } from "../src/runner.js";
import { SteeringChannel } from "../src/steering.js";
import { row, send } from "./gate-revision-harness.js";
import {
  NEW_TOKEN, OLD_TOKEN, PLAN_V2, askFrames, gateDrive, planFrames, type GateRun, type GateX,
} from "./gate-switch-harness.js";

installHarness();

const isV2Gate = (b: { status?: string; plan_md?: string }): boolean => b.status === "awaiting_approval" && b.plan_md === PLAN_V2;
const indexOf = (r: GateRun, event: string): number => r.events.indexOf(event);
const tokenScan = (r: GateRun): void => {
  const text = JSON.stringify([r.logs, api.messages(r.runId), r.states, r.acks.map(a => a.body)]);
  for (const token of [OLD_TOKEN, NEW_TOKEN]) assert.equal(text.includes(token), false, "no token value in logs, messages or reports");
};

/** The switch is requested DURING the bound human revision turn, with polls forced through checkpoint preparation and the
 *  in-flight awaiting_approval report (whose ACK also carries the stamp). */
function windowDrive(x: GateX, after: () => Promise<void>): Promise<void> {
  return (async () => {
    assert.ok(await x.waitFor(() => x.acks.some(a => a.body.status === "awaiting_approval") && x.waitTrace.length >= 1), "first gate waits");
    x.sendRevise();
    assert.ok(await x.waitFor(() => api.applyRequests.length >= 1), "the switch applied");
    assert.ok(await x.waitFor(() => x.acks.some(a => isV2Gate(a.body))), "the revised gate was acknowledged");
    await after();
  })();
}

for (const lane of ["checked", "ordinary"] as const) {
  describe(`issue #4806 m2 (1a): the window holds through the revised gate's applied ACK (${lane})`, { timeout: 90_000 }, () => {
    it("trips nothing before the applied ACK, applies after it, and leaves a usable gate", async () => {
      const r = await gateDrive({
        lane, control: true,
        duringRevisionTurn: async x => {
          x.requestSwitch("op-1", NEW_TOKEN, { onNextGateAck: true });
          await x.afterInputGets(3);
        },
        duringPrepare: async (x, call) => { if (call >= 1) await x.afterInputGets(3); },
        install: x => {
          // Hold the revised gate's ACK until inputs polls (carrying the stamp) went by while it was in flight.
          const held = (async () => { await x.stateSeen(isV2Gate); await x.afterInputGets(3); })();
          api.afterPersistState(x.runId, isV2Gate, held);
          return [];
        },
        drive: x => windowDrive(x, async () => {
          assert.ok(api.isApplied(x.runId, api.inputRows(x.runId).find(r => r.kind === "revise_plan")!.id), "the revise settled");
          send(x.runId, row("approve_plan"));
        }),
      });
      const ackV2 = r.events.findIndex((e, i) => e === "ack:awaiting_approval:applied" && r.events.slice(0, i).filter(v => v === "report:awaiting_approval").length >= 2);
      const apply = indexOf(r, "apply");
      assert.ok(ackV2 >= 0 && apply > ackV2, `the apply follows the revised gate's applied ACK: ${r.events.join(",")}`);
      assert.equal(r.events.filter(e => e === "apply").length, 1);
      const trip = indexOf(r, "trip");
      assert.ok(trip > ackV2, `nothing tripped before the applied ACK: ${r.events.join(",")}`);
      assert.equal(r.states.filter(s => s.status === "awaiting_approval" && s.plan_md === PLAN_V2).length, 1, "exactly one revised gate");
      assert.equal(r.defer.begins, r.defer.ends, "every window opened was closed");
      // Every turn before the apply used the old token, none after it.
      for (const t of r.turns) assert.equal(t.token, r.events.indexOf(`turn:${t.index}`) < apply ? OLD_TOKEN : NEW_TOKEN, `turn ${t.index}`);
      assert.deepEqual(r.captureErrors, []);
      assert.equal(r.entries.length, 1, "one executor entry: the same worker, generation and worktree throughout");
      assert.equal(r.reservations.length, 0, "no memory intervention");
      assert.equal(r.states.some(s => s.status === "recovery_wait"), false, "no memory-authority hold, release or reclaim");
      assert.equal(r.states.some(s => s.status === "completed"), true, "the approve after the apply was accepted");
      assert.equal(r.states.some(s => s.status === "credential_switch" || s.status === "failed"), false);
      tokenScan(r);
    });
  });
}

const RETAINED_LINE = "credential switch unconfirmed; retaining work and leaving the run non-terminal for requeue";
const logCount = (r: GateRun, msg: string): number => r.logs.filter(l => (l as { msg?: string }).msg === msg).length;

describe("issue #4806 m2 (1b): a switch requested while the revision's clarification is parked", { timeout: 90_000 }, () => {
  it("fails closed through the retained-stop arm with custody kept and nothing applied or released", async () => {
    let captures = 0, publications = 0;
    const r = await gateDrive({ lane: "checked", clarify: true, switchAtQuestion: true, approve: false, install: x => {
      x.spy(git, "commitWipMarker", (orig, _s, args) => { captures++; return orig(...args); });
      x.spy(client, "publishCheckpoint", (orig, _s, args) => { publications++; return orig(...args); });
      return [];
    } });
    assert.deepEqual(r.stops, [{ error: "CredentialSwitchRetainedStop", failClosed: true, early: false }], "the outer arm classified it");
    assert.equal(api.applyRequests.length, 0, "no apply request");
    assert.equal(r.states.some(s => s.status.startsWith("credential_switch")), false, "no credential_switch* report while parked or after");
    for (const t of r.turns) assert.equal(t.token, OLD_TOKEN, `turn ${t.index} kept the old credential`);
    assert.equal(logCount(r, RETAINED_LINE), 1, "the flight ended in the CredentialSwitchRetainedStop arm");
    const flight = r.flights[0]!;
    for (const flag of ["preserveRecoveryClone", "preserveSession", "keepGuardedInventoryOpen", "retainedLocalCustody"])
      assert.equal(flight[flag], true, `${flag} is kept`);
    assert.ok(fs.existsSync(r.entries[0]!.worktree), "the worktree is kept");
    assert.equal(r.states.some(s => ["completed", "failed", "cancelled"].includes(s.status) && s.failure_reason !== "run cancelled"), false, "no terminal report");
    assert.equal(await r.ledger.stateOf(r.runId, "op-1"), undefined, "no ledger entry");
    assert.deepEqual({ captures, publications }, { captures: 0, publications: 0 }, "no capture");
    assert.deepEqual({ begins: r.defer.begins, ends: r.defer.ends }, { begins: 1, ends: 1 }, "the window was released exactly once");
    assert.ok(r.admitted.length > 0, "memory admission was sampled from the stop decision");
    assert.equal(r.admitted.some(Boolean), false, "no guarded command admitted between the stop decision and the end of teardown");
    tokenScan(r);
  });
});

describe("issue #4806 m2 (1c): an automatic checker revise followed by a switch", { timeout: 90_000 }, () => {
  it("releases the turn-only window and trips at the next wait", async () => {
    const r = await gateDrive({ lane: "checked", autoRevise: true, control: true,
      duringRevisionTurn: async x => { x.requestSwitch("op-1", NEW_TOKEN); await x.afterInputGets(2); },
      drive: async x => {
        assert.ok(await x.waitFor(() => api.applyRequests.length >= 1), "the switch tripped and applied after the automatic revision");
      } });
    assert.equal(r.defer.begins, r.defer.ends, "the automatic revision's window was closed");
    assert.ok(indexOf(r, "trip") >= 0 && indexOf(r, "apply") > indexOf(r, "trip"), r.events.join(","));
    // The window closed with the turn: the switch tripped at the checker's wait, before any human gate existed.
    assert.ok(indexOf(r, "apply") < indexOf(r, "report:awaiting_approval"), r.events.join(","));
    assert.equal(r.states.some(s => s.status === "completed"), true, r.states.map(s => s.status).join(","));
    tokenScan(r);
  });
});

type Site = "turn" | "prep" | "report";
/** A bound human revision whose switch is requested at the turn, with `exit` fired at `site` of the revised gate. */
function exitAt(site: Site, exit: (x: GateX) => Promise<void> | void) {
  const opts = {
    lane: "checked" as const, control: true, approve: false,
    install: (x: GateX) => {
      if (site === "report") api.afterPersistState(x.runId, isV2Gate, (async () => {
        await x.stateSeen(isV2Gate);
        await exit(x);
        await x.afterInputGets(2);
      })());
      return [];
    },
    duringRevisionTurn: async (x: GateX) => {
      x.requestSwitch("op-1", NEW_TOKEN);
      if (site === "turn") { await exit(x); await x.afterInputGets(2); }
    },
    duringPrepare: async (x: GateX, call: number) => {
      if (site === "prep" && call >= 1) { await exit(x); await x.afterInputGets(2); }
    },
  };
  return opts;
}

describe("issue #4806 m2 (1d): every exit of the revision releases the window exactly once", { timeout: 90_000 }, () => {
  for (const site of ["turn", "prep", "report"] as const) {
    it(`a cancel during the ${site} ends the run cancelled and releases the window once`, async () => {
      const r = await gateDrive(exitAt(site, x => { x.cancel(); }));
      assert.deepEqual({ begins: r.defer.begins, ends: r.defer.ends }, { begins: 1, ends: 1 }, r.events.join(","));
      assert.equal(r.states.some(s => s.status === "failed" && s.failure_reason === "run cancelled"), true, r.states.map(s => `${s.status}:${s.failure_reason ?? ""}`).join(","));
      assert.equal(api.applyRequests.length, 0, "the stamp never applied on a cancelled run");
      // The cancel really landed at the named site.
      const reportsV2 = r.events.filter(e => e === "report:awaiting_approval").length;
      if (site === "turn") assert.equal(r.events.includes("prepare:begin:1"), false, "cancelled before the revised gate was prepared");
      if (site === "prep") assert.equal(r.events.includes("prepare:begin:1"), true, "cancelled inside the revised gate's checkpoint preparation");
      if (site === "report") assert.equal(reportsV2 >= 2, true, "cancelled while the revised gate's report was in flight");
    });
  }

  it("a ctx.gatePlan early exit (checkpoint preparation fails) releases the window once", async () => {
    const r = await gateDrive(exitAt("prep", () => { throw new Error("prepare failed"); }));
    assert.deepEqual({ begins: r.defer.begins, ends: r.defer.ends }, { begins: 1, ends: 1 });
    assert.equal(r.states.some(s => s.status === "failed" && /prepare failed/.test(s.failure_reason ?? "")), true,
      r.states.map(s => `${s.status}:${s.failure_reason ?? ""}`).join(","));
  });

  for (const lane of ["checked", "ordinary"] as const) {
    it(`a definitively refused presentation releases the window once (${lane})`, async () => {
      const r = await gateDrive({ ...exitAt("turn", () => undefined), lane,
        install: x => {
          api.answerStateOnce(x.runId, isV2Gate, { httpStatus: 409, runStatus: "awaiting_approval", gateRevision: 1, reason: "gate_presentation_conflict" });
          return [];
        } });
      assert.deepEqual({ begins: r.defer.begins, ends: r.defer.ends }, { begins: 1, ends: 1 }, r.events.join(","));
      assert.ok(r.events.indexOf("defer:end") < r.events.indexOf("trip") || !r.events.includes("trip"), "the window closed before any trip");
      const failure = r.states.map(s => s.failure_reason ?? s.recovery_cause ?? "").join("|");
      if (lane === "checked") assert.match(failure, /human-presentation ACK unrecoverable/);
      else assert.equal(r.logs.some(l => /api refused the gate presentation/.test((l as { msg?: string }).msg ?? "")), true, `the refusal path ran: ${failure}`);
    });
  }
});

describe("issue #4806 m2 (1e): a stamp polled during checkpoint preparation and publication breaks neither", { timeout: 90_000 }, () => {
  it("a witnessed revised gate is prepared, published and settled with the stamp pending", async () => {
    const r = await gateDrive({
      lane: "ordinary", control: true, witnessed: true, witnessAll: true, approve: false,
      duringRevisionTurn: async x => { x.requestSwitch("op-1", NEW_TOKEN, { onNextGateAck: true }); await x.afterInputGets(2); },
      duringPrepare: async (x, call) => { if (call >= 1) await x.afterInputGets(3); },
      install: x => {
        api.afterPersistState(x.runId, isV2Gate, (async () => { await x.stateSeen(isV2Gate); await x.afterInputGets(3); })());
        return [];
      },
      drive: async x => {
        assert.ok(await x.waitFor(() => x.acks.some(a => a.body.status === "awaiting_approval") && x.waitTrace.length >= 1));
        x.sendRevise();
        assert.ok(await x.waitFor(() => api.applyRequests.length >= 1), "the switch applied");
        x.cancel();
      } });
    const v2 = r.acks.find(a => isV2Gate(a.body))!;
    assert.ok(v2.body.gate_checkpoint_witness, "the revised gate carried its checkpoint witness");
    assert.equal(v2.ack.applied, true);
    const ackV2 = r.events.lastIndexOf("ack:awaiting_approval:applied", indexOf(r, "apply"));
    assert.ok(ackV2 >= 0 && indexOf(r, "prepare:end:1") < ackV2 && indexOf(r, "trip") > ackV2, r.events.join(","));
    assert.equal(r.states.some(s => s.status === "failed" && s.failure_reason !== "run cancelled"), false);
  });
});

// ---------------------------------------------------------------------------------------------
// (2) Tagged routing. A switch signal a RECEIPT failure raised inside the deferred window reaches the runner by one of
// two routes: (H) the executor's hook (runThroughSwitch -> ctx.attemptCredentialSwitch) or (O) executeClaim's outer arm
// (an unwrapped report). Each control is committed through the real input or entry point between the raise and the
// classification (the seam in the harness runs with the real tagged signal in hand).
// ---------------------------------------------------------------------------------------------
type Control = "cancel" | "pause" | "disk" | "fence" | "shutdown" | "receipt";

async function setControl(x: GateX, control: Control): Promise<void> {
  const st = x.steering();
  switch (control) {
    case "cancel": x.cancel(); await x.waitFor(() => st.isCancelled()); break;
    case "pause": send(x.runId, row("pause", "now")); await x.waitFor(() => st.getPauseMode() === "now"); break;
    case "disk": st.requestDiskStop(); break;
    case "fence":
      api.setInputClaimGeneration(x.runId, 99);
      send(x.runId, row("follow_up", "fence me"));
      await x.waitFor(() => st.claimFence() !== undefined);
      break;
    case "shutdown": x.runner.shutdown(); break;
    case "receipt":
      // The steering channel's own definitive receipt failure (no fake answer reaches it at this moment: the rows in
      // flight are parked, not applied).
      (st as unknown as { failReceipts(e: Error): void }).failReceipts(
        Object.assign(new Error("the applied receipt for 1 routed operator input(s) was refused: fixture"), { name: "InputReceiptError" }));
      break;
  }
}

/** Route H: the revised gate's receipt wait inside the window is refused with switch_pending. */
const hookRoute = (control: Control | undefined) => ({
  lane: "checked" as const, control: true,
  duringRevisionTurn: async (x: GateX) => {
    x.requestSwitch("op-1", NEW_TOKEN);
    api.failInputReceipts("applied", 409, "switch_pending");
    await x.afterInputGets(2);
  },
  ...(control ? { atHook: (x: GateX) => setControl(x, control) } : {}),
  drive: async (x: GateX) => {
    assert.ok(await x.waitFor(() => x.acks.some(a => a.body.status === "awaiting_approval") && x.waitTrace.length >= 1), "first gate waits");
    x.sendRevise();
    await x.waitFor(() => false, 15_000);
  },
});

const outerRoute = (control: Control | undefined) => ({
  lane: "checked" as const, clarify: true, switchAtQuestion: true,
  ...(control ? { atOuter: (x: GateX) => setControl(x, control) } : {}),
  drive: async (x: GateX) => {
    assert.ok(await x.waitFor(() => x.acks.some(a => a.body.status === "awaiting_approval") && x.waitTrace.length >= 1), "first gate waits");
    x.sendRevise();
    await x.waitFor(() => false, 15_000);
  },
});

interface Disposition {
  stop?: string;            // the classifier's typed error ("-" = the classifier was never reached)
  log?: string;             // the runLog line of the arm that took the control
  state?: string;           // a report that arm made
  failed?: RegExp;          // the visible failure
  noFailure?: boolean;
}
const dispositions: Record<Control, Disposition> = {
  cancel: { stop: "Error", state: "failed", failed: /^run cancelled$/ },
  pause: { stop: "PauseNowSignal", state: "pause_failed", log: "pause could not park this run outside the loop; leaving it for requeue", noFailure: true },
  disk: { stop: "DiskParkSignal", state: "recovery_wait", log: "run parked mid-run for its data volume; it resumes automatically", noFailure: true },
  fence: { stop: "StaleClaimError", log: "input receipt fenced this claim; stopping this flight", noFailure: true },
  shutdown: { stop: "Error", log: "run interrupted by worker shutdown; leaving it for requeue", noFailure: true },
  receipt: { stop: "Error", state: "failed", failed: /applied receipt for 1 routed operator input\(s\) was refused: fixture/ },
};

function assertDisposition(r: GateRun, control: Control, route: "H" | "O"): void {
  const want = dispositions[control];
  // The earlier arms of executeClaim own the fence and shutdown on the outer route: the classifier is never asked.
  const earlier = route === "O" && (control === "fence" || control === "shutdown");
  const early = route === "H";
  if (earlier) assert.deepEqual(r.stops, [], "the earlier arm took it before the classifier");
  else assert.deepEqual(r.stops, [{ error: want.stop, failClosed: false, early }], `the ${route} handler classified the ${control}: ${r.events.join(",")}`);
  assert.equal(api.applyRequests.length, 0, "never applied");
  assert.equal(r.events.includes("inplace"), false, "a tagged signal never enters inPlaceCredentialSwitch");
  assert.equal(r.states.some(s => s.status.startsWith("credential_switch")), false, "no switch report, no release");
  assert.equal(logCount(r, RETAINED_LINE), 0, "not the fail-closed stop");
  if (want.state) assert.equal(r.states.some(s => s.status === want.state), true, `${want.state} reported: ${r.states.map(s => s.status).join(",")}`);
  if (want.log) assert.equal(logCount(r, want.log), 1, `the arm logged "${want.log}"`);
  const failed = r.states.filter(s => s.status === "failed").map(s => s.failure_reason ?? "");
  if (want.failed) assert.match(failed.join("|"), want.failed);
  if (want.noFailure) assert.deepEqual(failed, [], "no failure report");
  assert.equal(failed.some(f => /plan cross-check:/.test(f)), false, "never a PlanCrossCheckFailure");
  assert.equal(r.defer.begins, r.defer.ends, "the window was released");
  tokenScan(r);
}

describe("issue #4806 m2 (2): tagged routing, hook route (H)", { timeout: 90_000 }, () => {
  it("fail closed: the typed retained stop, no apply, no release", async () => {
    const r = await gateDrive(hookRoute(undefined));
    assert.deepEqual(r.stops, [{ error: "CredentialSwitchRetainedStop", failClosed: true, early: true }], r.events.join(","));
    assert.equal(api.applyRequests.length, 0);
    assert.equal(r.events.includes("inplace"), false, "the hook never enters inPlaceCredentialSwitch for a tagged signal");
    assert.equal(r.states.some(s => s.status.startsWith("credential_switch")), false);
    assert.equal(logCount(r, RETAINED_LINE), 1);
    for (const flag of ["preserveRecoveryClone", "preserveSession", "keepGuardedInventoryOpen", "retainedLocalCustody"])
      assert.equal(r.flights[0]![flag], true, `${flag} is kept`);
    assert.equal(r.admitted.some(Boolean), false, "no guarded command admitted after the stop decision");
    tokenScan(r);
  });
  for (const control of ["cancel", "pause", "disk", "fence", "shutdown", "receipt"] as const) {
    it(`${control}: the committed control wins over the tagged switch signal`, async () => {
      assertDisposition(await gateDrive(hookRoute(control)), control, "H");
    });
  }
});

describe("issue #4806 m2 (2): tagged routing, outer arm (O)", { timeout: 90_000 }, () => {
  for (const control of ["cancel", "pause", "disk", "fence", "shutdown", "receipt"] as const) {
    it(`${control}: the committed control wins over the tagged switch signal`, async () => {
      const r = await gateDrive(outerRoute(control));
      assertDisposition(r, control, "O");
      // The outer arm hands the typed stop back through the dispatcher with the default denial restored: the
      // owner-pause arm can discharge it (it would stay denied had the restore been skipped).
      if (control === "pause") assert.equal(r.flights[0]!.sourceDispatchDenied, false, "sourceDispatchDenied restored before the recursion");
    });
  }
});


describe("issue #4806 m2 (3): untagged signals keep today's path", { timeout: 90_000 }, () => {
  it("an untagged in-place trip with a committed (not withheld) cancel enters inPlaceCredentialSwitch and ends as before", async () => {
    const r = await gateDrive({ lane: "ordinary", approve: false,
      beforeHook: async (x, tagged) => { if (!tagged) { x.cancel(); await x.waitFor(() => x.steering().isCancelled()); } },
      drive: async x => { await x.waitFor(() => false, 15_000); } });
    assert.deepEqual(r.stops, [], "the tagged classifier is not asked");
    assert.equal(r.events.includes("inplace"), true, `the hook entered inPlaceCredentialSwitch: ${r.events.join(",")}`);
    assert.equal(api.applyRequests.length, 0, "the committed cancel is honoured before any apply");
    assert.equal(r.states.some(s => s.status === "failed" && s.failure_reason === "run cancelled"), true, r.states.map(s => s.status).join(","));
    assert.equal(logCount(r, "retained recovery stopped; source and accounting kept"), 1, "today's retained-recovery cancel arm");
    assert.equal(logCount(r, RETAINED_LINE), 0);
  });

  it("an op-less v1 switch outside the window takes the v1 release path", async () => {
    const r = await gateDrive({ lane: "ordinary", opless: true, approve: false, drive: async x => { await x.waitFor(() => false, 15_000); } });
    assert.deepEqual(r.stops, []);
    assert.equal(r.events.includes("inplace"), true, "the hook entered inPlaceCredentialSwitch");
    assert.equal(api.applyRequests.length, 0, "no op id, so nothing to apply");
    assert.equal(r.states.some(s => s.status === "credential_switch"), true, `the v1 release report: ${r.states.map(s => s.status).join(",")}`);
    assert.equal(logCount(r, RETAINED_LINE), 0);
  });
});


// ---------------------------------------------------------------------------------------------
// (4) Renewal precedence: the control arrives while the apply is in flight (the server applied; the worker has not
// seen the answer yet), so it is committed when the applied arm decides whether to renew.
// ---------------------------------------------------------------------------------------------
const countContexts = (events: string[]): number => events.filter(e => e === "begin-context").length;
const contexts = (r: GateRun): number => countContexts(r.events);
const RETAINED_RECOVERY = "retained recovery stopped; source and accounting kept";

describe("issue #4806 m2 (4): renewal precedence at the applied switch", { timeout: 90_000 }, () => {
  const duringApplyFirst = (control: Control) => ({
    lane: "ordinary" as const,
    duringApply: (x: GateX) => setControl(x, control),
    // A pause or a stale claim leaves the flight at its gate: the end is the test's own cancel.
    drive: async (x: GateX) => {
      await x.waitFor(() => x.events.includes("apply:answered"));
      if (control === "pause" || control === "receipt") { x.cancel(); }
      await x.waitFor(() => false, 15_000);
    },
  });
  const ends: Record<string, (r: GateRun) => void> = {
    cancel: r => assert.equal(r.states.some(s => s.status === "failed" && s.failure_reason === "run cancelled"), true),
    pause: r => assert.equal(r.events.includes("inplace"), true),
    disk: r => assert.equal(r.states.some(s => s.status === "recovery_wait"), true),
    fence: r => assert.equal(logCount(r, "input receipt fenced this claim; stopping this flight"), 1),
    shutdown: r => assert.equal(logCount(r, "run interrupted by worker shutdown; leaving it for requeue"), 1),
  };
  for (const control of ["cancel", "pause", "disk", "fence", "shutdown"] as const) {
    it(`${control}: the applied arm keeps today's disposition, with no renewal and no retained recovery stop`, async () => {
      const r = await gateDrive({ ...duringApplyFirst(control), approve: false });
      assert.equal(r.events.includes("apply:answered"), true, "the switch applied");
      assert.equal(contexts(r), 1, `no renewal: ${r.events.join(",")}`);
      assert.equal(logCount(r, RETAINED_RECOVERY), 0, "no RetainedRecoveryStop");
      ends[control]!(r);
      assert.equal(r.reservations.length, 0);
      tokenScan(r);
    });
  }

  it("a milestone pause renews", async () => {
    const r = await gateDrive({ lane: "ordinary", approve: false,
      duringApply: async x => { send(x.runId, row("pause", "milestone")); await x.waitFor(() => x.steering().getPauseMode() === "milestone"); },
      drive: async x => { await x.waitFor(() => countContexts(x.events) >= 2); x.cancel(); await x.waitFor(() => false, 15_000); } });
    assert.equal(contexts(r), 2, r.events.join(","));
    assert.ok(r.events.indexOf("begin-context", r.events.indexOf("apply:answered")) > r.events.indexOf("apply:answered"), "renewed after the applied answer");
    assert.equal(logCount(r, RETAINED_RECOVERY), 0);
  });

  it("a newer pending op keeps the marker; the next applied arm renews", async () => {
    const r = await gateDrive({ lane: "ordinary", approve: false,
      duringApply: async x => {
        if (api.applyRequests.length !== 1) return;
        api.requestCredentialSwitch(x.runId, 7, "op-2");
        api.scriptApply(x.runId, { outcome: "applied", token: NEW_TOKEN });
        await x.waitFor(() => x.steering().pendingCredentialSwitchOp()?.opId === "op-2");
      },
      drive: async x => { await x.waitFor(() => countContexts(x.events) >= 2); x.cancel(); await x.waitFor(() => false, 15_000); } });
    const answered = r.events.map((e, i) => e === "apply:answered" ? i : -1).filter(i => i >= 0);
    assert.equal(answered.length, 2, "two applies: the first, then the newer op");
    const renewed = r.events.map((e, i) => e === "begin-context" ? i : -1).filter(i => i >= 0);
    assert.equal(renewed.length, 2, "the initial context and exactly one renewal");
    assert.ok(renewed[1]! > answered[1]!, `no renewal at the first applied arm (the newer op still pending); renewed after the second: ${r.events.join(",")}`);
    assert.deepEqual(api.applyRequests.map(a => a.body.op_id), ["op-1", "op-2"]);
    assert.equal(logCount(r, RETAINED_RECOVERY), 0);
  });
});

// ---------------------------------------------------------------------------------------------
// (5) Memory guards at the applied switch: each retires the parked context and ends through the retained recovery
// stop, with custody kept, no provider turn and no PlanCrossCheckFailure.
// ---------------------------------------------------------------------------------------------
type MemoryFlightLike = { incarnation?: unknown; held?: boolean; hold?: unknown; spent: number; ended?: boolean; context?: unknown };
const memoryOf = (x: GateX): MemoryFlightLike => (x.flight() as { workerMemory: MemoryFlightLike }).workerMemory;
/** `contexts`: how many memory contexts were begun in all (the initial one, plus the renewal when it got that far). */
const guards: Record<string, { set: (x: GateX) => Promise<void> | void; contexts: number }> = {
  // A re-registration also invalidates the memory authority, so a second case swaps only the incarnation the client
  // holds (the registration revision and listeners untouched), isolating the comparison the renewal guard makes.
  "an incarnation change by re-registration": { contexts: 1, set: async () => { api.memoryRegisterNonce = "m".repeat(43); await client.register("memory-gate-4806", "base"); } },
  "an incarnation change alone": { contexts: 1, set: () => {
    const holder = client as unknown as { memoryIncarnationValue: object };
    holder.memoryIncarnationValue = Object.freeze({ ...holder.memoryIncarnationValue, register_nonce: "z".repeat(43) });
  } },
  "memory held": { contexts: 1, set: x => { memoryOf(x).held = true; } },
  "a memory hold": { contexts: 1, set: x => { memoryOf(x).hold = Object.freeze({ context: undefined, deadline: Date.now() + 10_000, humanRevision: false, origin: undefined, interventionIds: Object.freeze([]) }); } },
  "a memory runtime invalidation": { contexts: 1, set: () => { client.revokeMemoryAuthority(); } },
  "an exhausted allowance at renewal": { contexts: 2, set: x => { memoryOf(x).spent = 2; } },
  "a holdPhase that returns no token": { contexts: 2, set: x => { x.spy(x.memory.commands, "holdPhase", () => undefined); } },
};

describe("issue #4806 m2 (5): memory guards at the applied switch", { timeout: 90_000 }, () => {
  for (const [name, guard] of Object.entries(guards)) {
    it(`${name}: retires the context and stops through the retained recovery arm`, async () => {
      const r = await gateDrive({ lane: "ordinary", approve: false, duringApply: x => Promise.resolve(guard.set(x)),
        drive: async x => { await x.waitFor(() => false, 15_000); } });
      assert.equal(r.events.includes("apply:answered"), true, "the switch applied");
      assert.equal(contexts(r), guard.contexts, r.events.join(","));
      assert.equal(logCount(r, RETAINED_RECOVERY), 1, "the retained recovery stop arm ended the flight");
      assert.equal(r.turns.length, 1, "no provider turn after the apply");
      assert.equal(r.states.some(s => s.status === "failed" && /plan cross-check/.test(s.failure_reason ?? "")), false, "no PlanCrossCheckFailure");
      assert.equal(r.states.some(s => s.status.startsWith("credential_switch")), false, "no release");
      assert.equal(r.states.some(s => s.status === "completed"), false);
      
      for (const flag of ["preserveRecoveryClone", "preserveSession", "keepGuardedInventoryOpen", "retainedLocalCustody"])
        assert.equal((r.x.flight() as Record<string, unknown>)[flag], true, `${flag} is kept`);
      assert.equal(memoryOf(r.x).context, undefined, "the context is retired");

      tokenScan(r);
    });
  }
});



// ---------------------------------------------------------------------------------------------
// (6) Controls
// ---------------------------------------------------------------------------------------------
const nextEvent = (r: GateRun, after: string): string | undefined => r.events[r.events.indexOf(after) + 1];

describe("issue #4806 m2 (6): controls", { timeout: 90_000 }, () => {
  for (const lane of ["checked", "ordinary"] as const) {
    it(`positive (${lane}): an ordinary human revise with no switch renews nothing and applies nothing`, async () => {
      const r = await gateDrive({ lane, control: true });
      assert.equal(api.applyRequests.length, 0);
      // The second context is the implementation turn's, begun only after the approved plan; nothing renews at the gate.
      assert.equal(contexts(r), 2);
      assert.ok(r.events.indexOf("begin-context", r.events.indexOf("begin-context") + 1) > r.events.indexOf("defer:end"), r.events.join(","));
      assert.deepEqual(r.defer, { begins: 1, ends: 1, depthSamples: [] });
      assert.equal(r.states.some(s => s.status === "completed"), true);
      assert.deepEqual(r.turns.map(t => t.token), [OLD_TOKEN, OLD_TOKEN, OLD_TOKEN]);
    });
  }

  for (const lane of ["checked", "ordinary"] as const) {
    it(`give-up (${lane}): no renewal, no new revision-recovery accept, the ordinary gate republishes as before`, async () => {
      const { PlanRevisionRecovery } = await import("../src/plan-revision-retry.js");
      let accepts = 0;
      const r = await gateDrive({ lane, retry: lane === "ordinary", approve: false, applyScript: [{ outcome: "not_applied" }],
        install: x => { x.spy(PlanRevisionRecovery.prototype, "accept", (orig, _s, args) => { accepts++; return orig(...args); }); return []; },
        drive: async x => {
          assert.ok(await x.waitFor(() => api.applyRequests.length >= 1), "the switch was attempted");
          assert.ok(await x.waitFor(() => x.waitTrace.filter(e => e === "enter").length >= 2), "the gate wait was re-entered");
          if (lane === "ordinary") send(x.runId, row("approve_plan"));
          else x.cancel();
          await x.waitFor(() => false, 15_000);
        } });
      const giveUp = r.events.indexOf("ack:credential_switch_failed:applied");
      assert.ok(giveUp > 0, "the give-up report");
      assert.equal(countContexts(r.events.slice(0, giveUp)), 1, `no renewal at a give-up: ${r.events.join(",")}`);
      assert.equal(countContexts(r.events.slice(giveUp, giveUp + 3)), 0, "and none right after it");
      if (lane === "ordinary") {
        assert.equal(accepts, 1, "only the approve verdict was accepted: the continuation added no accept");
        assert.equal(r.states.filter(s => s.status === "awaiting_approval").length, 2, "the ordinary gate was republished");
        assert.equal(r.events.includes("prepare:begin:1"), true);
      } else {
        assert.equal(accepts, 0, "the checked lane has no verdict to accept");
        assert.equal(r.states.filter(s => s.status === "awaiting_approval").length, 1, "the checked gate is re-joined, not republished");
      }
    });
  }

  it("an ask_user park at a non-gate site keeps base behaviour: the applied switch renews nothing and the next guarded turn stops", async () => {
    const r = await gateDrive({ lane: "ordinary", switchAtQuestion: true, approve: false, scriptOverride: { 0: askFrames, 1: planFrames(PLAN_V2) },
      drive: async x => { await x.waitFor(() => x.finished(), 15_000); } });
    assert.equal(r.events.includes("apply:answered"), true, "the switch applied at the question");
    assert.equal(contexts(r), 1, "no renewal at a non-gate site");
    assert.equal(logCount(r, RETAINED_RECOVERY), 1, "the parked context stays retired and the next guarded turn stops, as before the change");
    assert.equal(r.turns.length, 1);
  });
});

// ---------------------------------------------------------------------------------------------
// (7) Negatives
// ---------------------------------------------------------------------------------------------
describe("issue #4806 m2 (7): negatives", { timeout: 90_000 }, () => {
  it("an approve after an applied switch starts implementation only after the exact applied approve", async () => {
    let approveId = 0;
    const r = await gateDrive({ lane: "ordinary", approve: false,
      install: x => {
        x.spy(client, "applyInputs", async (orig, _s, args) => {
          const result = await orig(...args);
          if ((args[1] as number[]).includes(approveId)) x.events.push("approve:applied");
          return result;
        });
        return [];
      },
      drive: async x => {
        assert.ok(await x.waitFor(() => api.applyRequests.length >= 1), "applied");
        assert.ok(await x.waitFor(() => x.waitTrace.filter(e => e === "enter").length >= 2), "re-joined");
        approveId = send(x.runId, row("approve_plan"))[0]!.id;
        await x.waitFor(() => false, 15_000);
      } });
    // The second context is the renewal at the apply, the third the implementation turn's after the approve.
    assert.equal(contexts(r), 3, r.events.join(","));
    assert.ok(r.events.indexOf("begin-context", r.events.indexOf("apply:answered")) === r.events.indexOf("apply:answered") + 1, "renewed at the apply");
    assert.ok(api.isApplied(r.runId, approveId), "the approve applied");
    const applied = r.events.indexOf("approve:applied"), implement = r.events.indexOf("turn:1");
    assert.ok(applied >= 0 && implement > applied, `implementation starts after the approve applied: ${r.events.join(",")}`);
    assert.equal(r.turns[1]!.token, NEW_TOKEN);
    assert.equal(r.states.some(s => s.status === "completed"), true);
  });

  it("an automatic checker revise keeps no revision context; a bound human revise keeps one", async () => {
    const settled: Array<{ kind: string; automatic: boolean; inputId: boolean; kept: boolean }> = [];
    const install = (x: GateX) => {
      x.spy(RunRunner.prototype, "settleGateVerdict", (orig, _s, args) => {
        const out = orig(...args);
        const v = args[1] as { kind: string; automatic?: boolean; inputId?: number };
        const memory = (args[0] as { workerMemory?: { revisionContext?: unknown } }).workerMemory;
        settled.push({ kind: v.kind, automatic: v.automatic === true, inputId: v.inputId !== undefined, kept: memory?.revisionContext !== undefined });
        return out;
      });
      return [];
    };
    const auto = await gateDrive({ lane: "checked", autoRevise: true, control: true, install, drive: x => x.waitFor(() => x.finished()).then(() => undefined) });
    assert.equal(settled.some(v => v.kind === "revise" && v.kept), false, `an automatic revise records no revision context: ${JSON.stringify(settled)}`);
    assert.equal(auto.states.some(s => s.status === "completed"), true);
    settled.length = 0;
    await gateDrive({ lane: "checked", control: true, install });
    assert.deepEqual(settled.filter(v => v.kind === "revise"), [{ kind: "revise", automatic: false, inputId: true, kept: true }]);
  });
});

// ---------------------------------------------------------------------------------------------
// (8) After renewal; (9) renewal sends nothing
// ---------------------------------------------------------------------------------------------
describe("issue #4806 m2 (8, 9): the renewed context follows the replaced signals", { timeout: 90_000 }, () => {
  const afterRenewal = (control: Control | undefined, check: (x: GateX) => void | Promise<void>) => ({
    lane: "ordinary" as const, approve: false,
    drive: async (x: GateX) => {
      assert.ok(await x.waitFor(() => countContexts(x.events) >= 2), "the switch applied and the context was renewed");
      const cap = x.captured()!;
      assert.equal(cap.commands.isCurrent(cap.context, cap.flight, cap.lifecycleRevision), true, "the renewed context is current");
      assert.equal(cap.commands.admissionReason(cap.context), "phase-held", "it holds the gate's phase as the parked flight did");
      if (control) await setControl(x, control);
      await check(x);
      assert.equal(cap.commands.isCurrent(cap.context, cap.flight, cap.lifecycleRevision), false, "the renewed context is no longer current");
      assert.notEqual(cap.commands.admissionReason(cap.context), undefined, "and admits no guarded command");
      x.cancel();
      await x.waitFor(() => false, 15_000);
    },
  });
  for (const control of ["cancel", "pause", "disk"] as const) {
    it(`${control} on the replaced signals retires the renewed context and its command admission`, async () => {
      const r = await gateDrive(afterRenewal(control, async x => {
        assert.ok(await x.waitFor(() => memoryOf(x).context === undefined), `the renewed context was retired by the ${control}`);
      }));
      assert.equal(contexts(r), 2);
      assert.equal(nextEvent(r, "apply:answered"), "begin-context", "renewal sent no running report and no approval");
      tokenScan(r);
    });
  }

  it("repeated renewals leave no listener on any signal they watched", async () => {
    // Each watchMemoryContext call: the abort listeners it added, found by identity (present after, absent before).
    const calls: Array<Array<{ sig: AbortSignal; fn: unknown }>> = [];
    const live = (e: { sig: AbortSignal; fn: unknown }): boolean => getEventListeners(e.sig, "abort").includes(e.fn as never);
    const r = await gateDrive({ lane: "ordinary", approve: false,
      install: x => {
        x.spy(RunRunner.prototype, "watchMemoryContext", (orig, _s, args) => {
          const flight = args[0] as { cancel: AbortController; steering: { lifecycleSignal(): AbortSignal } };
          const signals = [flight.steering.lifecycleSignal(), flight.cancel.signal];
          const before = signals.map(sig => new Set(getEventListeners(sig, "abort")));
          const out = orig(...args);
          calls.push(signals.flatMap((sig, i) => getEventListeners(sig, "abort").filter(fn => !before[i]!.has(fn)).map(fn => ({ sig, fn }))));
          return out;
        });
        return [];
      },
      drive: async x => {
        assert.ok(await x.waitFor(() => countContexts(x.events) >= 2), "first renewal");
        x.requestSwitch("op-2", NEW_TOKEN);
        assert.ok(await x.waitFor(() => countContexts(x.events) >= 3), "second renewal");
        assert.equal(calls.length, 3, "initial watch plus two renewals");
        assert.deepEqual(calls.map(c => c.length), [2, 2, 2], "one lifecycle and one cancel listener per watch");
        assert.equal(calls[0]!.some(live), false, "the first context's listeners were removed");
        assert.equal(calls[1]!.some(live), false, "the first renewal's listeners were removed");
        assert.equal(calls[2]!.every(live), true, "only the latest watch is live");
        x.cancel();
        await x.waitFor(() => false, 15_000);
      } });
    assert.equal(contexts(r), 3);
    assert.equal(calls.flat().some(live), false, "no listener of any watch survives teardown");
  });
});

// ---------------------------------------------------------------------------------------------
// R1: the legacy lane's receipt wait of a revised gate runs once, on the confirming pass. A re-join of that gate after
// an applied switch (the window long closed) must not wait for receipts again. A cancel during such a wait could not
// be arranged: a cancel committed before the re-join is honoured by the apply hook itself, which never re-joins, and
// a cancel after it lands in the gate wait, so the repeated wait is pinned by counting its calls instead.
// ---------------------------------------------------------------------------------------------
describe("issue #4806 m2 (R1): the revised gate's receipt wait runs once", { timeout: 90_000 }, () => {
  it("an applied switch at the revised gate re-joins its wait without waiting for receipts again", async () => {
    let fromGate = 0;
    const r = await gateDrive({ lane: "ordinary", control: true, approve: false,
      install: x => {
        x.spy(SteeringChannel.prototype, "awaitReceiptSettlement", (orig, _s, args) => {
          if ((new Error().stack ?? "").includes("presentAndWait")) fromGate++;
          return orig(...args);
        });
        return [];
      },
      drive: async x => {
        assert.ok(await x.waitFor(() => x.acks.some(a => a.body.status === "awaiting_approval") && x.waitTrace.length >= 1), "first gate");
        x.sendRevise();
        assert.ok(await x.waitFor(() => x.acks.some(a => isV2Gate(a.body)) && x.waitTrace.filter(e => e === "enter").length >= 2), "revised gate waits");
        const before = fromGate;
        x.requestSwitch("op-1", NEW_TOKEN);
        assert.ok(await x.waitFor(() => x.waitTrace.filter(e => e === "enter").length >= 3), "the gate wait was re-joined after the apply");
        assert.equal(fromGate, before, "the re-join ran no receipt wait");
        send(x.runId, row("approve_plan"));
        await x.waitFor(() => x.finished());
      } });
    assert.equal(r.states.some(s => s.status === "failed"), false, r.states.map(s => `${s.status}:${s.failure_reason ?? ""}`).join(","));
  });
});
