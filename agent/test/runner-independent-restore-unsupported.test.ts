// Issue #3644 Gate B (amendment 15, m1): the three predecessor classes this release cannot restore from - canonical (an attempt-less
// source at the canonical clone path), hosted (this worker is a hosted pod) and missing_record (no boundary record for the dead
// incarnation) - park the run for its owner at `recovery_wait` / `retained_recovery_adoption_failed` instead of requeueing into the
// same refusal. Production RunRunner + real Git + the durable authority, driven through RunRunner.execute() with the shared rig
// (restore-route-rig.ts). Every row leaves P's journal, ledger, slot, clone, HOME and provision directory byte-identical.
import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { RequestError } from "../src/client.js";
import { api, client, git, installHarness } from "./runner-harness.js";
import {
  FEATURES, PARK_FEATURE, assertStoppedUntouched, buildRig, cfgAdd, cfgSet, cleanupRig, drive, failed, logged, refusals, resetRigRoots,
  rigDefaults, type Rig, type RigOptions,
} from "./restore-route-rig.js";
import { mintAttemptId } from "../src/run-quiescence.js";

installHarness();
rigDefaults.independentRestore = true;

afterEach(() => { cleanupRig(); });

const PARK_FEATURES = [...FEATURES, PARK_FEATURE];
const GUIDANCE = "this release does not support this predecessor";

interface FlightView { parked?: boolean; restoreUnsupportedHold?: boolean; runningAckStatus?: string; restorePredecessor?: unknown }

/** The flight the refusal disposition ran for, captured from the (private) refusal entry point. */
function spyFlight(r: Rig): { flight?: FlightView } {
  const seen: { flight?: FlightView } = {};
  const runner = r.runner as unknown as Record<string, (...a: unknown[]) => unknown>;
  const original = runner.refuseRestoreRoute!.bind(r.runner);
  mock.method(runner, "refuseRestoreRoute", (...args: unknown[]) => { seen.flight = args[1] as FlightView; return original(...args); });
  return seen;
}

/** Reports of the park: confirmed (the ACK carries the typed cause, as the real api's does) unless `hook` answers otherwise. */
function parkAcks(hook?: (body: Record<string, unknown>, original: () => Promise<unknown>) => Promise<unknown>, confirm = true): void {
  const real = client.reportState.bind(client) as unknown as (...a: unknown[]) => Promise<Record<string, unknown>>;
  mock.method(client, "reportState", (async (...args: unknown[]) => {
    const body = args[1] as Record<string, unknown>;
    const original = () => real(...args);
    const ack = (hook ? await hook(body, original) : await original()) as Record<string, unknown>;
    return confirm && body.status === "recovery_wait" && typeof body.recovery_cause === "string" ? { ...ack, recoveryCause: body.recovery_cause } : ack;
  }) as never);
}

const parks = (r: Rig) => api.states.filter(s => s.runId === r.runId && s.body.status === "recovery_wait");
const feed = (r: Rig): string[] => api.messages(r.runId).map(m => String((m.payload as { text?: string }).text ?? ""));
const guidance = (r: Rig): string[] => feed(r).filter(t => t.includes(GUIDANCE));
const itemSevenLines = (r: Rig): string[] => feed(r).filter(t => t.includes("independent restore refused"));

/** The refusal's frozen log contract: message, reason and class. */
function assertRefusal(r: Rig, reason: string, unsupported: string | undefined): Record<string, unknown> {
  const lines = refusals(r);
  assert.equal(lines.length, 1, JSON.stringify(r.logs.lines.filter(l => /restore|retained/i.test(String((l as { msg: string }).msg)))));
  assert.equal(lines[0]!.reason, reason);
  assert.equal(lines[0]!.unsupported, unsupported);
  return lines[0]!;
}

/** A confirmed park: one typed report at the claim's generation, the frozen logs and feed, the flight's flags. */
function assertHeld(r: Rig, seen: { flight?: FlightView }, unsupported: string): void {
  const sent = parks(r);
  assert.equal(sent.length, 1, "exactly one park report");
  assert.equal(sent[0]!.body.recovery_cause, "retained_recovery_adoption_failed");
  assert.equal(sent[0]!.body.claim_generation, r.claim.claim_generation);
  assert.equal(logged(r, "restore unsupported predecessor held").length, 1);
  assert.equal(logged(r, "restore unsupported predecessor held")[0]!.unsupported, unsupported);
  assert.equal(logged(r, "restore unsupported predecessor hold unconfirmed").length, 0);
  assert.equal(guidance(r).length, 1, "one guidance line");
  assert.ok(guidance(r)[0]!.includes(`(${unsupported})`));
  assert.ok(guidance(r)[0]!.length < 1200 && !guidance(r)[0]!.endsWith("..."), "bounded, not truncated");
  assert.equal(itemSevenLines(r).filter(t => !t.includes(GUIDANCE)).length, 0, "no item-7 line after a confirmed park");
  assert.equal(seen.flight?.parked, true, "the flight records the confirmed park");
  assert.equal(seen.flight?.restoreUnsupportedHold, true);
  assert.equal(seen.flight?.runningAckStatus, "running");
  // the park is requested only after the guidance reached the server
  const log = api.requestLog;
  assert.ok(log.indexOf("messages") >= 0 && log.indexOf("state:recovery_wait") > log.indexOf("messages"), log.join(","));
}

/** The unchanged item-7 refusal: one bounded line, no park, no guidance, no class on the log. */
function assertItemSeven(r: Rig, reason: string, unsupported?: string): void {
  assertRefusal(r, reason, unsupported);
  assert.equal(parks(r).length, 0, "no park report");
  assert.equal(guidance(r).length, 0, "no guidance");
  assert.equal(itemSevenLines(r).length, 1);
  assert.ok(itemSevenLines(r)[0]!.includes(reason) && itemSevenLines(r)[0]!.length < 400);
  assert.equal(logged(r, "restore unsupported predecessor held").length, 0);
}

async function held(unsupported: string, reason: string, o: RigOptions, extra?: (r: Rig) => Promise<void> | void): Promise<Rig> {
  const r = await buildRig({ features: PARK_FEATURES, ...o });
  await extra?.(r);
  const seen = spyFlight(r);
  parkAcks();
  const { before, traps } = await drive(r);
  assertRefusal(r, reason, unsupported);
  assertHeld(r, seen, unsupported);
  await assertStoppedUntouched(r, before, traps);
  return r;
}

describe("an unsupported predecessor parks the run for its owner (issue #3644 amendment 15)", () => {
  beforeEach(() => { resetRigRoots(); });

  it("canonical: an attempt-less journal at the canonical clone path", async () => {
    await held("canonical", "predecessor_not_plain", { pShape: "canonical" });
  });

  it("hosted: this worker is a hosted pod and P is an attempt", async () => {
    const r = await held("hosted", "hosted_restore_not_delivered", { sBoundary: "hosted" });
    assert.ok(guidance(r)[0]!.includes("blocks this worker's image roll until you cancel or discard it"));
    assert.equal(refusals(r)[0]!.canonical_candidate, undefined);
    assert.equal((refusals(r)[0]!.slot as { kind: string }).kind, "hosted");
  });

  it("hosted outranks canonical, and the log says the predecessor was a canonical candidate", async () => {
    const r = await held("hosted", "hosted_restore_not_delivered", { sBoundary: "hosted", pShape: "canonical" });
    assert.equal(refusals(r)[0]!.canonical_candidate, true);
  });

  it("missing_record: no boundary record for the predecessor's incarnation", async () => {
    const r = await held("missing_record", "predecessor_record_missing", { pBoundary: "missing" });
    assert.ok(!guidance(r)[0]!.includes("image roll"), "the hosted sentence is hosted-only");
  });
});

describe("negative controls: everything else keeps the unchanged refusal and sends no park (item 7)", () => {
  beforeEach(() => { resetRigRoots(); });

  async function item7(reason: string, o: RigOptions, extra?: (r: Rig) => Promise<void> | void): Promise<Rig> {
    const r = await buildRig({ features: PARK_FEATURES, ...o });
    await extra?.(r);
    parkAcks();
    const { before, traps } = await drive(r);
    assertItemSeven(r, reason);
    assert.equal(failed(r).length, 0);
    await assertStoppedUntouched(r, before, traps);
    return r;
  }

  it("a compose worker with a hosted predecessor record", () => item7("hosted_restore_not_delivered", { pBoundary: "hosted" }).then(() => undefined));
  it("an invalid predecessor record", () => item7("predecessor_record_invalid", { pBoundary: "invalid" }).then(() => undefined));
  it("a predecessor record of a failed cross-check", () => item7("cross_check_failed", { pBoundary: "failed" }).then(() => undefined));

  describe("a canonical-shaped journal that fails an absence check is not a canonical candidate", () => {
    it("a stopped marker of the run", () => item7("conflicting_attribution", { pShape: "canonical" }, r => { cfgAdd(r, `uzi-stopped.${r.runId}.entry`, "{}"); })
      .then(() => undefined));
    it("a protected descriptor key whose value names neither the run nor the clone path", () => item7("conflicting_attribution", { pShape: "canonical" }, r => {
      cfgAdd(r, `uzi-retained.${r.runId}.journal`, "{}");
    }).then(() => undefined));
    it("another key naming the predecessor's path", () => item7("conflicting_attribution", { pShape: "canonical" }, r => {
      cfgSet(r, "uzi-recovery.agent/issue-9998.clone", JSON.stringify({ runId: "another-run", clonePath: r.pPath }));
    }).then(() => undefined));
    it("a live ledger entry of the same run", () => item7("conflicting_attribution", { pShape: "canonical" }, r => {
      const id = mintAttemptId(2);
      cfgAdd(r, r.ledgerKey, JSON.stringify({ attemptId: id, runId: r.runId, clonePath: `${r.pPath}.attempt-${id}`, state: "live" }));
    }).then(() => undefined));
    it("an incomplete attribution config read", () => item7("attribution_unverifiable", { pShape: "canonical" }, () => {
      mock.method(git, "readRestoreAttribution", async () => ({ entries: [], complete: false }));
    }).then(() => undefined));
  });

  it("a hosted worker whose own record does not match its live identity (hidden procfs) is not hosted-classified", () =>
    item7("procfs_hidden", { sBoundary: "hosted", sLive: "hidden" }).then(() => undefined));
  it("a hosted worker with a pending footprint on the predecessor's slot is a pending refusal", () =>
    item7("predecessor_pending", { sBoundary: "hosted" }, async r => {
      await r.pRt.withTransaction(() => r.pRt.hooks.beginDestructive([r.pPath], "unsettled-destruction"));
    }).then(() => undefined));
});

describe("the park is sent only when its preconditions hold", () => {
  beforeEach(() => { resetRigRoots(); });

  it("an api that does not advertise the cause: the class is logged, nothing is parked, the refusal line is the unchanged one", async () => {
    const r = await buildRig({ pShape: "canonical" });
    parkAcks();
    const seen = spyFlight(r);
    const { before, traps } = await drive(r);
    assertItemSeven(r, "predecessor_not_plain", "canonical");
    assert.equal(seen.flight?.restoreUnsupportedHold, undefined);
    await assertStoppedUntouched(r, before, traps);
  });

  it("a first running report whose ACK status is not running: nothing is parked", async () => {
    const r = await buildRig({ pShape: "canonical", features: PARK_FEATURES });
    let first = true;
    api.stateAckWire = (body, run) => {
      if (body.status === "running" && first) { first = false; return { status: 200, body: { run: { ...run, status: "paused" } } }; }
      return undefined;
    };
    const seen = spyFlight(r);
    const { before, traps } = await drive(r);
    assert.equal(seen.flight?.runningAckStatus, "paused");
    assertItemSeven(r, "predecessor_not_plain", "canonical");
    await assertStoppedUntouched(r, before, traps);
  });
});

describe("the park acknowledgement", () => {
  beforeEach(() => { resetRigRoots(); });

  const unconfirmed = (r: Rig): void => {
    assert.equal(logged(r, "restore unsupported predecessor hold unconfirmed").length, 1);
    assert.equal(logged(r, "restore unsupported predecessor held").length, 0);
  };

  it("a 400 from the api: the hold is unconfirmed, the guidance is corrected by the unchanged refusal line, nonterminal", async () => {
    const r = await buildRig({ pShape: "canonical", features: PARK_FEATURES });
    parkAcks(async (body, original) => {
      if (body.status === "recovery_wait") throw new RequestError("POST", "/state", 400, "unsupported recovery_cause");
      return original();
    });
    const seen = spyFlight(r);
    const { before, traps } = await drive(r);
    unconfirmed(r);
    assertRefusal(r, "predecessor_not_plain", "canonical");
    assert.equal(parks(r).length, 0);
    assert.equal(guidance(r).length, 1);
    assert.equal(itemSevenLines(r).filter(t => !t.includes(GUIDANCE)).length, 1, "the corrective line follows the guidance");
    assert.notEqual(seen.flight?.parked, true);
    assert.equal(seen.flight?.restoreUnsupportedHold, true);
    await assertStoppedUntouched(r, before, traps);
  });

  it("a lost ACK followed by a stale_claim replay: the stale claim reaches its own arm, no unconfirmed-hold log, no item-7 line, nothing is failed", async () => {
    const r = await buildRig({ pShape: "canonical", features: PARK_FEATURES });
    let reports = 0;
    parkAcks(async (body, original) => {
      if (body.status === "recovery_wait" && ++reports === 1) {
        await original();
        api.failStateWhen(r.runId, b => b.status === "recovery_wait", { disposition: "stale_claim", runStatus: "queued" });
        throw new Error("connection reset after the server applied the park");
      }
      return original();
    });
    const seen = spyFlight(r);
    const { before, traps } = await drive(r);
    assert.equal(reports, 2, "the lost report was replayed once");
    assert.equal(logged(r, "restore unsupported predecessor hold unconfirmed").length, 0, "a stale claim is not an unresolved hold");
    assert.equal(logged(r, "restore unsupported predecessor held").length, 0);
    assert.equal(logged(r, "run claim superseded server-side (stale_claim); stopping this flight").length, 1, "the stale-claim arm ran");
    assert.equal(itemSevenLines(r).length, 0, "no refusal line is emitted for a run another claim may own");
    assert.equal(seen.flight?.restoreUnsupportedHold, true);
    assert.notEqual(seen.flight?.parked, true, "a stale replay confirms nothing");
    assert.equal(failed(r).length, 0);
    await assertStoppedUntouched(r, before, traps);
  });

  it("a lost ACK followed by a lost ownership probe: the ownership loss propagates, no unconfirmed-hold log, no item-7 line, nothing is failed", async () => {
    const r = await buildRig({ pShape: "canonical", features: PARK_FEATURES });
    parkAcks(async (body, original) => {
      if (body.status === "recovery_wait") {
        api.setOwnershipNotOwned(r.runId);
        throw new Error("connection reset before the park was acknowledged");
      }
      return original();
    });
    const seen = spyFlight(r);
    const { before, traps } = await drive(r);
    assert.equal(logged(r, "restore unsupported predecessor hold unconfirmed").length, 0, "a lost ownership is not an unresolved hold");
    assert.equal(logged(r, "restore unsupported predecessor held").length, 0);
    assert.equal(itemSevenLines(r).length, 0, "no refusal line is emitted for a run another claim may own");
    assert.notEqual(seen.flight?.parked, true);
    assert.equal(failed(r).length, 0);
    await assertStoppedUntouched(r, before, traps);
  });

  it("an ACK that names another status (lost eligibility, here claimed) is an unconfirmed hold", async () => {
    const r = await buildRig({ pShape: "canonical", features: PARK_FEATURES });
    parkAcks(async (body, original) => {
      const ack = await original() as Record<string, unknown>;
      return body.status === "recovery_wait" ? { ...ack, status: "claimed" } : ack;
    });
    const seen = spyFlight(r);
    const { before, traps } = await drive(r);
    unconfirmed(r);
    assert.notEqual(seen.flight?.parked, true);
    assert.equal(seen.flight?.restoreUnsupportedHold, true);
    await assertStoppedUntouched(r, before, traps);
  });

  it("a status-only ACK without the typed cause is unresolved: probe, replay, probe, then an unconfirmed hold", async () => {
    const r = await buildRig({ pShape: "canonical", features: PARK_FEATURES });
    parkAcks(undefined, false);
    const seen = spyFlight(r);
    const { before, traps } = await drive(r);
    unconfirmed(r);
    assert.equal(parks(r).length, 2, "the replay was sent once");
    assert.notEqual(seen.flight?.parked, true);
    assert.equal(failed(r).length, 0);
    await assertStoppedUntouched(r, before, traps);
  });

  it("a stop before the send (the worker shuts down while the guidance is flushed): nothing is parked and the guidance was conditional", async () => {
    const r = await buildRig({ pShape: "canonical", features: PARK_FEATURES });
    const real = client.postMessages.bind(client);
    mock.method(client, "postMessages", (async (...args: Parameters<typeof real>) => {
      const out = await real(...args);
      r.runner.shutdown();
      return out;
    }) as never);
    const { before, traps } = await drive(r);
    assert.equal(parks(r).length, 0, "the stop came before the send");
    assert.equal(logged(r, "restore unsupported predecessor held").length, 0);
    assert.equal(guidance(r).length, 1, "conditional guidance was emitted before the send was stopped");
    for (const text of guidance(r)) {
      assert.ok(text.includes("The worker is requesting an owner hold; the run's status shows whether it was applied"));
      assert.ok(text.includes("If not held, automatic retries follow the ordinary requeue accounting"));
    }
    assert.equal(failed(r).length, 0);
    await assertStoppedUntouched(r, before, traps);
  });
});
