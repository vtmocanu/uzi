import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { WorkerClient } from "../src/client.js";
import { RUN_REGISTRATION_EVIDENCE_FEATURE } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const fixture = JSON.parse(readFileSync(new URL("../../fixtures/worker-protocol/registration-evidence-running.json", import.meta.url), "utf8")) as {
  status: string; claim_generation: number;
  registration_evidence: { register_nonce: string; claim_generation: number; registration_epoch: number };
};
const NONCE = fixture.registration_evidence.register_nonce;
const GEN = fixture.claim_generation;
const EPOCH = fixture.registration_evidence.registration_epoch;
const FEATURES = ["claim_generation_fence", RUN_REGISTRATION_EVIDENCE_FEATURE];
const ok = JSON.stringify({ run: { id: "r1", status: "running" } });

type Reply = "strict" | "ok" | { status: number; body: string } | "lost";

async function make(replies: Reply[], opts: { features?: string[]; nonce?: string | null } = {}) {
  const wires: string[] = [];
  let registrations = 0;
  const transport: typeof fetch = async (url, init) => {
    if (String(url).endsWith("/register")) {
      registrations++;
      return new Response(JSON.stringify({
        worker_id: "11111111-1111-4111-8111-111111111111",
        ...(opts.nonce === null ? {} : { register_nonce: opts.nonce ?? NONCE }),
        protocol_features: opts.features ?? FEATURES,
      }));
    }
    wires.push(String(init?.body));
    const reply = replies.shift();
    assert.ok(reply, "unexpected additional request");
    if (reply === "lost") throw new TypeError("lost ACK");
    if (reply === "ok") return new Response(ok);
    if (reply === "strict") return new Response('{"error":"invalid request body"}', { status: 400 });
    return new Response(reply.body, { status: reply.status });
  };
  const c = new WorkerClient("http://fixture.invalid", "fixture", "test", nullLogger(), {
    fetch: transport, sleep: async () => {}, terminalRetrySchedule: [0],
  });
  await c.register("fixture", "base", 1, undefined, ["credential_switch_v1"], undefined, 0);
  return { c, wires, registrations: () => registrations };
}

const parsed = (wires: string[]) => wires.map(w => JSON.parse(w) as Record<string, unknown>);

describe("registration evidence on the running report", () => {
  it("emits exactly the shared fixture body", async () => {
    const { c, wires } = await make(["ok"]);
    await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { registrationEpoch: EPOCH });
    assert.deepEqual(parsed(wires), [fixture]);
  });

  it("logs and returns the server's ack value without changing behavior", async () => {
    const { c } = await make([{ status: 200, body: JSON.stringify({ run: { id: "r1", status: "running" }, registration_evidence: "refused" }) }]);
    const ack = await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { registrationEpoch: EPOCH });
    assert.equal(ack.registrationEvidence, "refused");
    assert.equal(ack.applied, true);
  });

  const omissions: [string, { features?: string[]; nonce?: string | null }, number | undefined, number | undefined][] = [
    ["feature not negotiated", { features: ["claim_generation_fence"] }, GEN, EPOCH],
    ["no register nonce", { nonce: null }, GEN, EPOCH],
    ["no epoch supplied", {}, GEN, undefined],
    ["generation 0", {}, 0, EPOCH],
    ["generation undefined", {}, undefined, EPOCH],
    ["negative epoch", {}, GEN, -1],
    ["fractional epoch", {}, GEN, 1.5],
    ["NaN epoch", {}, GEN, Number.NaN],
    ["unsafe epoch", {}, GEN, Number.MAX_SAFE_INTEGER + 1],
    ["fractional generation", {}, 1.5, EPOCH],
  ];
  for (const [name, setup, gen, epoch] of omissions) {
    it(`omits evidence: ${name}`, async () => {
      const { c, wires } = await make(["ok"], setup);
      await c.reportState("r1", { status: "running", ...(gen === undefined ? {} : { claim_generation: gen }) }, undefined,
        epoch === undefined ? undefined : { registrationEpoch: epoch });
      assert.equal(wires.length, 1);
      assert.equal("registration_evidence" in parsed(wires)[0]!, false);
    });
  }

  it("epoch 0 is a valid witness", async () => {
    const { c, wires } = await make(["ok"]);
    await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { registrationEpoch: 0 });
    assert.deepEqual((parsed(wires)[0]!.registration_evidence as { registration_epoch: number }).registration_epoch, 0);
  });

  it("never attaches to a non-running report, nor keeps a caller-supplied copy", async () => {
    const { c, wires } = await make(["ok", "ok"]);
    await c.reportState("r1", { status: "awaiting_approval", plan_md: "p", claim_generation: GEN }, undefined, { registrationEpoch: EPOCH });
    await c.reportState("r1", { status: "failed", failure_reason: "x", claim_generation: GEN,
      registration_evidence: { register_nonce: "n", claim_generation: GEN, registration_epoch: EPOCH } }, undefined, { registrationEpoch: EPOCH });
    assert.equal(wires.length, 2);
    for (const w of parsed(wires)) assert.equal("registration_evidence" in w, false);
  });

  describe("mixed-version strict-decode fallback", () => {
    it("resends once with ONLY the evidence removed, then omits it until re-register", async () => {
      const { c, wires, registrations } = await make(["strict", "ok", "ok", "ok"]);
      const opts = { registrationEpoch: EPOCH };
      await c.reportState("r1", { status: "running", claim_generation: GEN, session_id: "s" }, undefined, opts);
      const [first, second] = parsed(wires);
      assert.ok(first!.registration_evidence);
      const expected = structuredClone(first!);
      delete expected.registration_evidence;
      assert.deepEqual(second, expected, "claim_generation and every other field are unchanged");
      assert.equal(second!.claim_generation, GEN);
      assert.equal(wires.length, 2);
      // Sticky: the next running report omits it, and a feature refresh does not reopen it.
      c.protocolFeatures = FEATURES;
      await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, opts);
      assert.equal("registration_evidence" in parsed(wires)[2]!, false);
      // Re-register restores it.
      await c.register("fixture", "base", 1, undefined, ["credential_switch_v1"], undefined, 0);
      assert.equal(registrations(), 2);
      await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, opts);
      assert.deepEqual(parsed(wires)[3], fixture);
    });

    it("a second 400 on the resend is not retried a third time", async () => {
      const { c, wires } = await make(["strict", "strict", "ok"]);
      await assert.rejects(c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { registrationEpoch: EPOCH }));
      assert.equal(wires.length, 2);
      assert.equal("registration_evidence" in parsed(wires)[1]!, false);
      assert.equal(parsed(wires)[1]!.claim_generation, GEN, "the generation strip never runs after the evidence strip");
    });

    for (const [name, reply] of [
      ["unrelated 400", { status: 400, body: '{"error":"invalid plan"}' }],
      ["similar-prose 400", { status: 400, body: '{"error":"invalid request body for this plan"}' }],
      ["409", { status: 409, body: JSON.stringify({ run: { id: "r1", status: "claimed" } }) }],
    ] as const) {
      it(`${name} sends no evidence-stripping resend`, async () => {
        const { c, wires } = await make([reply, "ok"]);
        await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { registrationEpoch: EPOCH }).catch(() => undefined);
        assert.equal(wires.length, 1);
        assert.ok(parsed(wires)[0]!.registration_evidence);
        await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { registrationEpoch: EPOCH }).catch(() => undefined);
        assert.ok(parsed(wires).at(-1)!.registration_evidence, "feature not dropped");
      });
    }

    it("a 5xx retries the identical evidence-bearing body", async () => {
      const { c, wires } = await make([{ status: 503, body: '{"error":"invalid request body"}' }, "ok"]);
      await c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { registrationEpoch: EPOCH });
      assert.equal(wires.length, 2);
      assert.equal(wires[0], wires[1]);
      assert.ok(parsed(wires)[1]!.registration_evidence);
    });

    it("a transport error retries the identical body, never an evidence strip", async () => {
      const { c, wires } = await make(["lost", "lost"]);
      await assert.rejects(c.reportState("r1", { status: "running", claim_generation: GEN }, undefined, { registrationEpoch: EPOCH }));
      assert.equal(wires.length, 2);
      assert.equal(wires[0], wires[1]);
    });

    it("an abort after the strict refusal sends no resend", async () => {
      const abort = new AbortController();
      const { c, wires } = await make([{ status: 400, body: '{"error":"invalid request body"}' }]);
      const sent = c.reportState("r1", { status: "running", claim_generation: GEN }, abort.signal, { registrationEpoch: EPOCH });
      abort.abort(new Error("cancelled"));
      await assert.rejects(sent);
      assert.ok(wires.length <= 1);
    });
  });
});
