// PRD #1795 M3: plan-gate verdicts bound to the gate revision they were sent against. These drive a
// real WorkerClient + SteeringChannel against FakeApi rows carrying the api's persisted binding
// (gate_binding / gate_revision), and assert the matching table black-box:
//
//   bound(N), N === confirmed revision        → taken
//   bound(N), N <  confirmed revision         → disposed as stale
//   bound(N), N >  confirmed, or none yet     → kept buffered (the B1 window)
//   unbound approve on a human-gated claim    → disposed on arrival (discard lane, notice)
//   unbound reject / revise                   → legacy epoch handling (A2)
//   malformed binding                         → fails closed, never legacy
//   legacy (no binding)                       → today's epoch / replay-cutoff handling
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { FakeApi } from "./fake-api.js";
import { nullLogger } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { SteeringChannel, type PlanVerdict } from "../src/steering.js";
import type { UserInput } from "../src/protocol.js";

const TOKEN = "worker-join-token-0123456789";
const RUN = "gate-revision-run";
const GEN = 4;
let api: FakeApi;
let baseUrl: string;
const channels: SteeringChannel[] = [];

const STALE_APPROVE_NOTICE = "Approval ignored — the plan changed; re-send if you still want it.";
const UNBOUND_APPROVE_NOTICE =
  "Approval ignored — it was sent while no plan was awaiting approval; approve the plan once it is shown.";
const MALFORMED_BINDING_NOTICE =
  "A plan verdict with an unreadable gate binding was ignored — re-send it if you still want it.";
const GATE_CLOSED_NOTICE = "The plan gate has already closed — this plan verdict was ignored.";
const REPLAY_STALE_VERDICT_NOTICE = "A plan verdict sent before this plan was shown was ignored — re-send it if you still want it.";

beforeEach(async () => {
  api = new FakeApi(TOKEN);
  api.strictReceiptGenerations = true;
  api.setInputClaimGeneration(RUN, GEN);
  baseUrl = await api.listen();
});

afterEach(async () => {
  await Promise.all(channels.splice(0).map((ch) => ch.stop()));
  await api.close();
});

interface Harness {
  ch: SteeringChannel;
  notices: string[];
  cancel: AbortController;
}

function harness(opts: { humanGated?: boolean; start?: boolean } = {}): Harness {
  const client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), {
    sleep: async () => {},
    terminalRetrySchedule: [1, 1, 1],
  });
  const notices: string[] = [];
  const cancel = new AbortController();
  const ch = new SteeringChannel(client, RUN, 1, nullLogger(), cancel, {
    claimGeneration: GEN,
    notify: (text) => notices.push(text),
  });
  ch.setHumanGated(opts.humanGated ?? true);
  channels.push(ch);
  if (opts.start !== false) ch.start();
  return { ch, notices, cancel };
}

let nextId = 500;
function row(kind: UserInput["kind"], extra: Partial<UserInput> = {}, body: string | null = null): UserInput {
  return { id: nextId++, kind, body, ...extra };
}
const bound = (revision: number): Partial<UserInput> => ({ gate_binding: "bound", gate_revision: revision });
const UNBOUND: Partial<UserInput> = { gate_binding: "unbound" };

const tick = (ms = 2): Promise<void> => new Promise((r) => setTimeout(r, ms));

async function until(ready: () => boolean, what = "condition reached"): Promise<void> {
  for (let i = 0; i < 1000 && !ready(); i++) await tick();
  assert.ok(ready(), what);
}

/** Send `rows` and wait until the channel ACKed (so routed) every one of them. */
async function deliver(...rows: UserInput[]): Promise<void> {
  api.appendInputs(RUN, rows);
  const ids = rows.map((r) => r.id);
  await until(
    () => ids.every((id) => api.inputReceiptReplies.length > 0 && api.inputReceiptCalls.some((c) => c.kind === "ack" && c.ids.includes(id))),
    "the rows were ACKed",
  );
  // The ACK reply is routed on the same tick it lands; give the loop a few ticks to finish.
  await tick(20);
}

/** Resolve to the gate event, or "pending" if none arrives within `ms`. */
async function gateEvent(ch: SteeringChannel, epoch = ch.currentEpoch(), ms = 80): Promise<PlanVerdict | "pending"> {
  return Promise.race([ch.awaitGateEvent(epoch), tick(ms).then(() => "pending" as const)]);
}

const receiptIds = (kind: "applied" | "discarded"): number[] =>
  api.inputReceiptCalls.filter((c) => c.kind === kind).flatMap((c) => c.ids);

describe("bound verdicts match the confirmed gate revision exactly (PRD #1795)", () => {
  // B1 at the channel: a verdict routed while the report's ACK is in flight (after persistence,
  // so the api bound it to the new revision) is taken once the ACK confirms that revision, even
  // though the gate epoch was bumped after it was routed.
  for (const c of [
    { kind: "approve_plan" as const, body: null, expect: { kind: "approve", selection: { status: "absent" } } },
    { kind: "reject_plan" as const, body: "not this one", expect: { kind: "reject", reason: "not this one" } },
    { kind: "revise_plan" as const, body: "smaller steps", expect: { kind: "revise", feedback: "smaller steps" } },
  ]) {
    it(`B1: a ${c.kind} bound to the gate being reported is taken after its ACK, across the epoch bump`, async () => {
      const { ch } = harness();
      ch.clearGateRevision(); // gatePlan entry
      const r = row(c.kind, bound(2), c.body);
      await deliver(r);
      assert.equal(await gateEvent(ch), "pending", "nothing is confirmed yet: the bound verdict waits");
      ch.setGateRevision(2); // the applied ACK's gate_revision
      ch.bumpEpoch(); // the revision gate's bump, AFTER the verdict was routed
      const v = await gateEvent(ch);
      assert.notEqual(v, "pending", "the verdict bound to the confirmed revision was taken");
      assert.deepStrictEqual(c.kind === "revise_plan" ? { ...(v as PlanVerdict), inputId: undefined } : v, c.kind === "revise_plan" ? { ...c.expect, inputId: undefined } : c.expect);
      assert.ok(!receiptIds("discarded").includes(r.id), "never discarded");
    });
  }

  it("the same race with a LEGACY approve goes stale at the bump (the pre-#1795 defect, kept for legacy rows)", async () => {
    const { ch, notices } = harness();
    const r = row("approve_plan");
    await deliver(r);
    ch.setGateRevision(2);
    ch.bumpEpoch();
    assert.equal(await gateEvent(ch), "pending");
    assert.deepStrictEqual(notices, [STALE_APPROVE_NOTICE]);
    await until(() => receiptIds("discarded").includes(r.id), "the stale legacy approve is discarded, never applied");
  });

  it("B2: an approve bound to the previous gate stays stale when polled after the transition", async () => {
    const { ch, notices } = harness();
    ch.setGateRevision(3);
    const r = row("approve_plan", bound(2));
    await deliver(r);
    assert.deepStrictEqual(notices, [STALE_APPROVE_NOTICE], "disposed on arrival with the stale notice");
    assert.equal(await gateEvent(ch), "pending", "the gate at revision 3 is not approved");
    await until(() => receiptIds("discarded").includes(r.id), "the stale approve goes to the discard lane");
    assert.ok(!receiptIds("applied").includes(r.id), "never applied (an applied approve is the human approval)");
  });

  it("B2: a bound reject and revise below the confirmed revision are final (applied), never taken", async () => {
    const { ch } = harness();
    ch.setGateRevision(3);
    const reject = row("reject_plan", bound(1), "old");
    const revise = row("revise_plan", bound(2), "old feedback");
    await deliver(reject, revise);
    assert.equal(await gateEvent(ch), "pending");
    await until(() => receiptIds("applied").includes(reject.id) && receiptIds("applied").includes(revise.id), "both applied on their own");
  });

  it("B3: a bound(N) verdict never acts while the next report is declined (nothing confirmed)", async () => {
    const { ch } = harness();
    ch.setGateRevision(1);
    const revise = row("revise_plan", bound(1), "rework it");
    await deliver(revise);
    const v = await gateEvent(ch);
    assert.equal(v !== "pending" && v.kind, "revise", "the revise at the confirmed revision is taken");
    // Mid-revision: the api still shows gate 1 (awaiting_approval), so a new approve binds to 1.
    const approve = row("approve_plan", bound(1));
    await deliver(approve);
    // The revised plan's report (revision 2) is DECLINED: gatePlan cleared the revision at entry
    // and no applied ACK confirmed one.
    ch.clearGateRevision();
    ch.bumpEpoch();
    assert.equal(await gateEvent(ch, ch.currentEpoch(), 150), "pending", "the mid-revision approve of plan 1 never approves anything");
    assert.ok(!receiptIds("applied").includes(approve.id));
    // A later accepted gate at revision 2 disposes of it as stale.
    ch.setGateRevision(2);
    assert.equal(await gateEvent(ch), "pending");
    await until(() => receiptIds("discarded").includes(approve.id), "stale once revision 2 is confirmed");
  });

  it("taking a revise clears the confirmed revision: a verdict bound to that gate then waits", async () => {
    const { ch } = harness();
    ch.setGateRevision(5);
    await deliver(row("revise_plan", bound(5), "again"));
    const v = await gateEvent(ch);
    assert.equal(v !== "pending" && v.kind, "revise");
    await deliver(row("approve_plan", bound(5)));
    assert.equal(await gateEvent(ch), "pending", "no revision is confirmed during the revision turn");
  });

  it("a bound verdict above the confirmed revision waits; a buffered newer one is not superseded by an older", async () => {
    const { ch, notices } = harness();
    ch.setGateRevision(4);
    const newer = row("approve_plan", bound(5));
    await deliver(newer);
    const older = row("reject_plan", bound(4), "late");
    await deliver(older);
    // bound(4) is AT the confirmed revision, so it supersedes nothing newer: a buffered bound(5)
    // is higher, so the older one is stale on arrival instead.
    assert.deepStrictEqual(notices, ["Rejection ignored — the plan changed; re-send if you still want it."]);
    assert.equal(await gateEvent(ch), "pending", "bound(5) waits for revision 5");
    ch.setGateRevision(5);
    const v = await gateEvent(ch);
    assert.equal(v !== "pending" && v.kind, "approve");
  });

  it("a cancel still wins over a waiting bound verdict", async () => {
    const { ch } = harness();
    await deliver(row("approve_plan", bound(7)), row("cancel"));
    assert.deepStrictEqual(await gateEvent(ch), { kind: "cancel" });
  });

  it("a bound verdict after the gate closed is disposed of with the closed notice", async () => {
    const { ch, notices } = harness();
    ch.closeGate("reject");
    const r = row("approve_plan", bound(1));
    await deliver(r);
    assert.deepStrictEqual(notices, [GATE_CLOSED_NOTICE]);
    await until(() => receiptIds("discarded").includes(r.id));
  });

  it("a bound verdict bypasses the replay cutoff that disposes of a legacy one", async () => {
    api.stampInputCreatedAt = true;
    const { ch, notices } = harness({ start: false });
    ch.setReplayCutoff("2999-01-01T00:00:00.000000Z"); // every row predates it
    ch.setGateRevision(2);
    ch.start();
    const legacy = row("reject_plan", {}, "legacy");
    await deliver(legacy);
    assert.deepStrictEqual(notices, [REPLAY_STALE_VERDICT_NOTICE], "the legacy reject is replay-stale");
    await deliver(row("approve_plan", bound(2)));
    const v = await gateEvent(ch);
    assert.equal(v !== "pending" && v.kind, "approve", "the bound approve matched the confirmed revision");
  });

  it("an unjudged claim still takes a bound verdict (the fail-closed window is for legacy rows)", async () => {
    const { ch, notices } = harness({ start: false });
    assert.equal(ch.setReplayCutoff(undefined), false);
    ch.setGateRevision(3);
    ch.start();
    await deliver(row("reject_plan", bound(3), "no"));
    assert.deepStrictEqual(notices, []);
    assert.deepStrictEqual(await ch.takeResumedGateEvent(), { kind: "reject", reason: "no" }, "a pending bound reject acts before the gate is re-presented");
  });
});

describe("unbound verdicts (PRD #1795 B5, A2)", () => {
  it("an unbound approve on a human-gated claim is disposed of through the discard lane with a notice", async () => {
    const { ch, notices } = harness();
    const r = row("approve_plan", UNBOUND);
    await deliver(r);
    assert.deepStrictEqual(notices, [UNBOUND_APPROVE_NOTICE]);
    ch.setGateRevision(1);
    assert.equal(await gateEvent(ch), "pending", "it never approves the gate shown later");
    await until(() => receiptIds("discarded").includes(r.id));
    assert.ok(!receiptIds("applied").includes(r.id));
  });

  it("an unbound reject keeps the legacy handling: it stops the run at its first gate", async () => {
    const { ch } = harness();
    await deliver(row("reject_plan", UNBOUND, "stop now"));
    ch.setGateRevision(1); // the first gate: no bump
    assert.deepStrictEqual(await gateEvent(ch), { kind: "reject", reason: "stop now" });
  });

  it("unbound revise feedback sent before the first gate is taken at it", async () => {
    const { ch } = harness();
    const r = row("revise_plan", UNBOUND, "cover the edge case");
    await deliver(r);
    ch.setGateRevision(1);
    assert.deepStrictEqual(await gateEvent(ch), { kind: "revise", feedback: "cover the edge case", inputId: r.id });
  });

  it("an unbound reject still goes stale across a revision bump (legacy epoch path)", async () => {
    const { ch } = harness();
    await deliver(row("reject_plan", UNBOUND, "old"));
    ch.bumpEpoch();
    assert.equal(await gateEvent(ch), "pending");
  });

  it("on an autopilot claim every binding reads as legacy (no human gate)", async () => {
    const { ch, notices } = harness({ humanGated: false });
    await deliver(row("approve_plan", UNBOUND));
    assert.deepStrictEqual(notices, []);
    assert.deepStrictEqual(await gateEvent(ch), { kind: "approve", selection: { status: "absent" } });
  });

  it("a claim turning human-gated (a forced ci_fix gate) disposes of a buffered unbound approve", async () => {
    const { ch, notices } = harness({ humanGated: false });
    const r = row("approve_plan", UNBOUND);
    await deliver(r);
    ch.setHumanGated(true);
    assert.deepStrictEqual(notices, [UNBOUND_APPROVE_NOTICE]);
    assert.equal(await gateEvent(ch), "pending");
    await until(() => receiptIds("discarded").includes(r.id));
  });
});

describe("malformed bindings fail closed (PRD #1795)", () => {
  const shapes: Array<{ name: string; extra: Record<string, unknown> }> = [
    { name: "an unknown binding", extra: { gate_binding: "sort-of" } },
    { name: "bound without a revision", extra: { gate_binding: "bound" } },
    { name: "bound at revision 0", extra: { gate_binding: "bound", gate_revision: 0 } },
    { name: "bound at a negative revision", extra: { gate_binding: "bound", gate_revision: -3 } },
    { name: "bound at a fractional revision", extra: { gate_binding: "bound", gate_revision: 1.5 } },
    { name: "bound at a string revision", extra: { gate_binding: "bound", gate_revision: "2" } },
    { name: "unbound with a revision", extra: { gate_binding: "unbound", gate_revision: 2 } },
    { name: "a revision without a binding", extra: { gate_revision: 2 } },
    { name: "an explicit null binding", extra: { gate_binding: null } },
  ];
  for (const shape of shapes) {
    it(`${shape.name}: an approve is disposed of (discarded), a reject and a revise are final (applied)`, async () => {
      const { ch, notices } = harness();
      ch.setGateRevision(2);
      const approve = row("approve_plan", shape.extra as Partial<UserInput>);
      const reject = row("reject_plan", shape.extra as Partial<UserInput>, "no");
      const revise = row("revise_plan", shape.extra as Partial<UserInput>, "change");
      await deliver(approve, reject, revise);
      assert.deepStrictEqual(notices, [MALFORMED_BINDING_NOTICE, MALFORMED_BINDING_NOTICE, MALFORMED_BINDING_NOTICE]);
      assert.equal(await gateEvent(ch), "pending", "a malformed verdict never acts, whatever the epoch or revision");
      await until(() => receiptIds("discarded").includes(approve.id), "the approve goes to the discard lane");
      await until(
        () => receiptIds("applied").includes(reject.id) && receiptIds("applied").includes(revise.id),
        "the reject and revise are applied on their own",
      );
      assert.ok(!receiptIds("applied").includes(approve.id));
    });
  }
});

describe("legacy rows keep the epoch behaviour (PRD #1795 D3)", () => {
  it("a legacy approve queued before the first gate applies at epoch 0 whatever the confirmed revision", async () => {
    const { ch } = harness();
    await deliver(row("approve_plan"));
    ch.setGateRevision(9);
    assert.deepStrictEqual(await gateEvent(ch), { kind: "approve", selection: { status: "absent" } });
  });

  it("a legacy revise at the current epoch beats a legacy approve in the same batch", async () => {
    const { ch } = harness();
    const revise = row("revise_plan", {}, "first");
    await deliver(row("approve_plan"), revise);
    assert.deepStrictEqual(await gateEvent(ch), { kind: "revise", feedback: "first", inputId: revise.id });
  });

  it("a legacy revise from a prior epoch is stale", async () => {
    const { ch } = harness();
    await deliver(row("revise_plan", {}, "old"));
    ch.bumpEpoch();
    assert.equal(await gateEvent(ch), "pending");
  });
});
