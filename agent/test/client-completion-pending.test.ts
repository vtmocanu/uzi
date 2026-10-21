import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readRunAck, WorkerClient } from "../src/client.js";
import { decodeCompletionPending, sameCompletionPending } from "../src/completion-pending.js";
import { nullLogger } from "./helpers.js";

// PRD #1230 M4: completion_pending decoding on the state ACK, the completion permit and the claim.
// Absent is legacy; any present value that fails the strict bounded decode is malformed (fail closed).
const CRIT = "00000000-0000-4000-8000-0000000000a1";
const criterion = (over: Record<string, unknown> = {}) => ({
  criterion_id: CRIT, milestone_id: "m2", kind: "maintainer", text: "Observe the hosted rollout",
  reason: "Only the owner can reach the cluster", conditions: ["Rolled out"], verification_steps: ["Open the dashboard"],
  authorization_revision: 1, status: "unverified", ...over,
});
const pending = (over: Record<string, unknown> = {}, criteria: unknown[] = [criterion()]) =>
  ({ version: 1, contract_revision: 3, criteria, ...over });
const RUN = "11111111-1111-4111-8111-111111111111";
const run = { id: "run", status: "running", completion_revision: 3, completion_deferred: [], completion_attempts: 0 };

const malformed: [string, unknown][] = [
  ["null", null], ["string", "x"], ["array", []], ["unknown top field", pending({ extra: 1 })],
  ["version 2", pending({ version: 2 })], ["revision 0", pending({ contract_revision: 0 })],
  ["unsafe revision", pending({ contract_revision: Number.MAX_SAFE_INTEGER + 1 })],
  ["criteria missing", { version: 1, contract_revision: 3 }],
  ["too many criteria", pending({}, Array.from({ length: 257 }, (_, i) =>
    criterion({ criterion_id: `00000000-0000-4000-8000-${String(i).padStart(12, "0")}` })))],
  ["unknown criterion field", pending({}, [criterion({ extra: true })])],
  ["code kind", pending({}, [criterion({ kind: "code" })])],
  ["satisfied status", pending({}, [criterion({ status: "satisfied" })])],
  ["authorization 0", pending({}, [criterion({ authorization_revision: 0 })])],
  ["authorization fractional", pending({}, [criterion({ authorization_revision: 1.5 })])],
  ["non-uuid criterion", pending({}, [criterion({ criterion_id: "c1" })])],
  ["milestone charset", pending({}, [criterion({ milestone_id: "-m" })])],
  ["milestone length", pending({}, [criterion({ milestone_id: "m".repeat(65) })])],
  ["blank text", pending({}, [criterion({ text: "  " })])],
  ["text over 2048 bytes", pending({}, [criterion({ text: "x".repeat(2049) })])],
  ["multibyte text over 2048 bytes", pending({}, [criterion({ text: "é".repeat(1025) })])],
  ["reason over 2048", pending({}, [criterion({ reason: "x".repeat(2049) })])],
  ["NUL in text", pending({}, [criterion({ text: "a\u0000b" })])],
  ["no conditions", pending({}, [criterion({ conditions: [] })])],
  ["nine conditions", pending({}, [criterion({ conditions: Array(9).fill("c") })])],
  ["condition over 512", pending({}, [criterion({ conditions: ["x".repeat(513)] })])],
  ["no steps", pending({}, [criterion({ verification_steps: [] })])],
  ["step over 512", pending({}, [criterion({ verification_steps: ["x".repeat(513)] })])],
  ["duplicate criterion ids", pending({}, [criterion(), criterion()])],
];

describe("completion_pending decoder", () => {
  it("absent is legacy; an empty projection is a valid, distinct value", () => {
    assert.deepEqual(decodeCompletionPending(undefined), { kind: "absent" });
    assert.equal(decodeCompletionPending(pending({}, [])).kind, "ok");
  });
  it("accepts the maximum 256 criteria and the byte bounds exactly", () => {
    const criteria = Array.from({ length: 256 }, (_, i) => criterion({
      criterion_id: `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`, text: "x".repeat(2048),
      reason: "y".repeat(2048), conditions: Array(8).fill("c".repeat(512)), verification_steps: Array(8).fill("v".repeat(512)),
    }));
    assert.equal(decodeCompletionPending(pending({}, criteria)).kind, "ok");
  });
  for (const [name, value] of malformed) {
    it("malformed: " + name, () => assert.deepEqual(decodeCompletionPending(value), { kind: "malformed" }));
  }
  it("equality ignores order, but sees a single authorization revision, a missing side and a changed field", () => {
    const a = decodeCompletionPending(pending({}, [criterion(), criterion({ criterion_id: "00000000-0000-4000-8000-0000000000a2" })]));
    const b = decodeCompletionPending(pending({}, [criterion({ criterion_id: "00000000-0000-4000-8000-0000000000a2" }), criterion()]));
    const c = decodeCompletionPending(pending({}, [criterion({ authorization_revision: 2 }), criterion({ criterion_id: "00000000-0000-4000-8000-0000000000a2" })]));
    assert.ok(a.kind === "ok" && b.kind === "ok" && c.kind === "ok");
    assert.equal(sameCompletionPending(a.pending, b.pending), true);
    assert.equal(sameCompletionPending(a.pending, c.pending), false);
    assert.equal(sameCompletionPending(a.pending, undefined), false);
    assert.equal(sameCompletionPending(undefined, undefined), true);
  });
});

describe("state ACK completion_pending", () => {
  it("absent ACK carries no projection and no malformed marker", async () => {
    const ack = await readRunAck(new Response(JSON.stringify({ run })));
    assert.equal("completionPending" in ack, false);
    assert.equal("completionPendingMalformed" in ack, false);
  });
  it("a valid projection is decoded beside, and independent of, the legacy deferred ids", async () => {
    const ack = await readRunAck(new Response(JSON.stringify({ run: { ...run, completion_pending: pending() } })));
    assert.deepEqual(ack.completionPending, pending());
    assert.deepEqual(ack.completionProjection, { revision: 3, deferredIds: [] }, "criterion ids are never merged into deferredIds");
  });
  for (const [name, value] of malformed) {
    it("malformed ACK projection (" + name + ") is marked and never degrades to absence", async () => {
      const ack = await readRunAck(new Response(JSON.stringify({ run: { ...run, completion_pending: value } })));
      assert.equal(ack.completionPendingMalformed, true);
      assert.equal(ack.completionPending, undefined);
    });
  }
  it("reportState surfaces both outcomes on the StateAck", async t => {
    let body: unknown = { run: { ...run, completion_pending: pending() } };
    t.mock.method(globalThis, "fetch", async () => Response.json(body));
    const client = new WorkerClient("http://example.com", "fake-worker-token", "test", nullLogger(), { terminalRetrySchedule: [], sleep: async () => {} });
    assert.deepEqual((await client.reportState("run", { status: "running" })).completionPending, pending());
    body = { run: { ...run, completion_pending: { version: 9 } } };
    assert.equal((await client.reportState("run", { status: "running" })).completionPendingMalformed, true);
  });
  it("a maximally escaped projection fits a running ACK only under the server's pending header", async t => {
    // Every text byte escapes to six wire bytes, as in api TestSemanticCompletionPendingWireBound.
    const text = "\u0001".repeat(2048), step = "\u0001".repeat(512);
    const max = pending({}, Array.from({ length: 256 }, (_, i) => criterion({
      criterion_id: `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`, milestone_id: "m".repeat(64),
      text, reason: text, conditions: Array(8).fill(step), verification_steps: Array(8).fill(step),
    })));
    const wire = JSON.stringify({ run: { ...run, completion_pending: max } });
    assert.ok(wire.length > 18 * 1024 * 1024 && wire.length < 6 * 1024 * 1024 + 20 * 1024 * 1024);
    let respond = () => new Response(wire, { headers: { "X-Uzi-Completion-Pending": "v1" } });
    t.mock.method(globalThis, "fetch", async () => respond());
    const client = new WorkerClient("http://example.com", "fake-worker-token", "test", nullLogger(), { terminalRetrySchedule: [], sleep: async () => {} });
    assert.equal((await client.reportState("run", { status: "running" })).completionPending?.criteria.length, 256);
    // Without the header the structural 6 MiB cap still applies.
    respond = () => new Response(wire);
    await assert.rejects(client.reportState("run", { status: "running" }), /exceeds/);
    // The header grants a bounded allowance, not an unbounded read.
    respond = () => new Response(wire + " ".repeat(8 * 1024 * 1024), { headers: { "X-Uzi-Completion-Pending": "v1" } });
    await assert.rejects(client.reportState("run", { status: "running" }), /exceeds/);
    // A header without a projection, or an unknown version, fails closed.
    for (const [version, payload] of [["v1", { run }], ["v2", { run: { ...run, completion_pending: pending() } }]] as const) {
      respond = () => new Response(JSON.stringify(payload), { headers: { "X-Uzi-Completion-Pending": version } });
      assert.equal((await client.reportState("run", { status: "running" })).completionPendingMalformed, true, version);
    }
  });
});

describe("completion permit completion_pending", () => {
  const permit = (extra: Record<string, unknown>) => ({ granted: true, permit: {
    id: "p", contract_revision: 3, branch: "agent/issue-1", head: "h", issued_at: "2026-10-20T00:00:00Z", finding_ids: [],
    completion_scope: {}, ...extra } });
  const ask = async (t: { mock: { method: typeof import("node:test").mock.method } }, body: unknown) => {
    t.mock.method(globalThis, "fetch", async () => Response.json(body));
    const client = new WorkerClient("http://example.com", "fake-worker-token", "test", nullLogger(), { terminalRetrySchedule: [], sleep: async () => {} });
    return client.requestCompletionPermit("run", { contractRevision: 3, branch: "agent/issue-1", head: "h" });
  };
  it("absent, present and malformed", async t => {
    const absent = await ask(t, permit({}));
    assert.equal(absent.permit?.completionPending, undefined);
    assert.equal(absent.permit?.completionPendingMalformed, undefined);
    const present = await ask(t, permit({ completion_pending: pending() }));
    assert.deepEqual(present.permit?.completionPending, pending());
    for (const [, value] of malformed) {
      const bad = await ask(t, permit({ completion_pending: value }));
      assert.equal(bad.permit?.completionPendingMalformed, true);
      assert.equal(bad.permit?.completionPending, undefined);
    }
  });
});

describe("claim config completion_pending", () => {
  const claimBody = (config: Record<string, unknown>) => JSON.stringify({
    run_id: RUN, issue_iid: 1, issue_title: "t", issue_description: "d", repo: {}, secrets: {}, last_seq: 0, agents: [], config,
  });
  const claim = async (t: { mock: { method: typeof import("node:test").mock.method } }, config: Record<string, unknown>) => {
    t.mock.method(globalThis, "fetch", async () => new Response(claimBody(config), { status: 200 }));
    const client = new WorkerClient("http://example.com", "fake-worker-token", "test", nullLogger(), { terminalRetrySchedule: [], sleep: async () => {} });
    return client.claimRun();
  };
  it("absent stays absent; present is decoded", async t => {
    assert.equal("completion_pending" in ((await claim(t, { contract_revision: 3 }))?.config ?? {}), false);
    assert.deepEqual((await claim(t, { completion_pending: pending() }))?.config?.completion_pending, pending());
  });
  for (const [name, value] of malformed) {
    it("a malformed claim projection (" + name + ") fails the claim closed", async t => {
      await assert.rejects(claim(t, { completion_pending: value }), /malformed completion_pending/);
    });
  }
});
